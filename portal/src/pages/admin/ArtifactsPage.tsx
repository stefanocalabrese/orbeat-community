import { useState, type ChangeEvent } from "react";
import {
  useAcknowledgeFindings,
  useAdminArtifact,
  useAdminArtifacts,
  useArtifactPinningEnabled,
  useArtifactRevisions,
  useCreateArtifact,
  useDeleteArtifact,
  useMarketplacePublish,
  useMarketplaceStatus,
  useRollbackArtifact,
  useSetArtifactMinRevision,
  useUpdateArtifact,
  useSubmitArtifact,
  useWithdrawArtifact,
} from "../../api/queries";
import type { AdminArtifact, ArtifactInput, ArtifactRevision, ArtifactRoleGrants, ScanFinding } from "../../api/types";
import { errMsg } from "../../api/client";
import { useAuth } from "../../auth/useAuth";
import { FormField, inputCls } from "../../components/FormField";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { StateBadge, Pill, Chip } from "../../components/ui/Badge";
import { Card, Panel } from "../../components/ui/Card";
import { Diff } from "../../components/ui/Diff";
import { ListSearchBox, SortableTh } from "../../components/ui/AdminListControls";
import { ConflictNotice } from "./ConflictNotice";
import { isConflict } from "./conflict";
import { SEARCH_DEBOUNCE_MS, useDebouncedValue } from "../../hooks/useDebouncedValue";
import {
  distributedIdentity,
  distributedSummary,
  pendingIdentity,
  rollbackConfirmMessage,
  type IdentityChange,
  type IdentityField,
} from "./identity";

const EMPTY: ArtifactInput = {
  type: "skill",
  name: "",
  description: "",
  content: "",
  memoryScope: "",
  memorySeed: "",
  version: "",
  visibility: "org",
};

interface ArtifactFormProps {
  initial: ArtifactInput;
  onSubmit: (v: ArtifactInput) => void;
  pending: boolean;
  error: string;
  /** Set only on the edit form — create can never 412 (no If-Match sent). */
  conflict?: boolean;
  onReload?: () => void;
  submitLabel: string;
  /**
   * The artifact's existing per-role grants, from the by-id fetch. Edit form
   * only: a create has no grants yet, so it passes nothing.
   */
  roleGrants?: ArtifactRoleGrants;
  /**
   * The identity fields this artifact has saved but not yet distributed
   * (pendingIdentity). Edit form only: a create has nothing distributed yet,
   * so it passes nothing and the note never renders.
   */
  identityGap?: IdentityChange[];
}

/**
 * The warning an admin needs before switching an artifact from org visibility
 * back to role.
 *
 * Grants are never deleted when an artifact goes org-visibility; they go
 * dormant, because only role-visibility artifacts are distributed through
 * entitlements. Switching back revives every one of them at once, so an admin
 * who flipped to org months ago and is now narrowing access can silently
 * restore a list of roles they believed was gone. The server derives the count
 * (it rides on GET /v1/admin/artifacts/{id}); counting client-side from the
 * artifact-entitlements list would undercount on exactly the artifacts with the
 * most grants, since that list is capped at 100 rows.
 *
 * Shown only for the org -> role direction. That is the moment the number
 * changes who receives the artifact, and it is the moment it is still
 * preventable.
 */
function ReviveWarning({ grants }: { grants: ArtifactRoleGrants }) {
  return (
    <p role="status" className="rounded-md border border-warn bg-warn-weak px-3 py-2 text-sm text-text">
      Saving revives {grants.count} dormant {grants.count === 1 ? "grant" : "grants"} on this artifact:{" "}
      <span className="font-medium">{grants.roles.join(", ")}</span>
      {grants.truncated && <> and more</>}. They were kept when it was switched to org visibility and take effect again
      when this change reaches distribution. Remove them on the Artifact entitlements page first if that is not what you
      want.
    </p>
  );
}

/**
 * What developers receive while a saved identity edit waits to be distributed.
 *
 * Deliberately phrased around the reader's disk rather than around approval
 * state. The portal cannot tell which edition it is talking to, and the
 * sentence has to stay true in both: in Community the update and its
 * auto-approval commit together, so there is never a gap and this never
 * renders; in Enterprise it is the only thing on screen that explains why the
 * file on a developer's machine still has the old name.
 */
function PendingIdentityNote({ changes }: { changes: IdentityChange[] }) {
  if (changes.length === 0) {
    return null;
  }
  return (
    <p role="note" className="rounded-md border border-warn bg-warn-weak px-3 py-2 text-sm text-text">
      Developers still receive <span className="font-medium">{distributedSummary(changes)}</span> until the saved
      changes are distributed.
    </p>
  );
}

function ArtifactForm({ initial, onSubmit, pending, error, conflict = false, onReload, submitLabel, roleGrants, identityGap }: ArtifactFormProps) {
  const [v, setV] = useState(initial);
  // `initial.visibility` is the SAVED value (the form is constructed from the
  // by-id fetch and never re-primed), so this compares what is about to be
  // written against what is stored, not against an earlier keystroke.
  const willRevive =
    initial.visibility === "org" && v.visibility === "role" && (roleGrants?.count ?? 0) > 0;

  function set(k: keyof ArtifactInput) {
    return (e: ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>) => {
      const next = { ...v, [k]: e.target.value };
      if (k === "type" && e.target.value !== "subagent") {
        next.memoryScope = "";
        next.memorySeed = "";
      }
      if (k === "memoryScope" && e.target.value !== "user" && e.target.value !== "project") {
        next.memorySeed = "";
      }
      setV(next);
    };
  }

  return (
    <Card className="mt-4 max-w-xl p-5">
      <form
        className="grid gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          onSubmit(v);
        }}
      >
        <PendingIdentityNote changes={identityGap ?? []} />
        <FormField label="Type" htmlFor="f-type">
          <select id="f-type" aria-label="Type" className={inputCls} value={v.type} onChange={set("type")}>
            <option value="skill">skill</option>
            <option value="subagent">subagent</option>
            <option value="rule">rule</option>
          </select>
        </FormField>
        <FormField label="Name" htmlFor="f-name">
          <input id="f-name" aria-label="Name" className={inputCls} value={v.name} onChange={set("name")} required />
        </FormField>
        <FormField label="Description" htmlFor="f-desc">
          <input id="f-desc" aria-label="Description" className={inputCls} value={v.description} onChange={set("description")} />
        </FormField>
        <FormField
          label="Content"
          htmlFor="f-content"
          hint={
            v.type === "rule"
              ? "Plain markdown — no YAML frontmatter; delivered verbatim into each project's AGENTS.md"
              : "Markdown with YAML frontmatter (name + description required)"
          }
        >
          <textarea
            id="f-content"
            aria-label="Content"
            className={`${inputCls} min-h-32 font-mono`}
            value={v.content}
            onChange={set("content")}
            required
          />
        </FormField>
        <FormField
          label="Visibility"
          htmlFor="f-visibility"
          hint="org = everyone via the native plugin; role = entitled roles only (sync client)"
        >
          <select id="f-visibility" aria-label="Visibility" className={inputCls} value={v.visibility} onChange={set("visibility")}>
            <option value="org">org</option>
            <option value="role">role</option>
          </select>
        </FormField>
        {willRevive && roleGrants && <ReviveWarning grants={roleGrants} />}
        {v.type === "subagent" && (
          <FormField label="Memory scope" htmlFor="f-memory" hint="Native memory injected into subagent frontmatter; 'off' disables it">
            <select id="f-memory" aria-label="Memory scope" className={inputCls} value={v.memoryScope} onChange={set("memoryScope")}>
              <option value="">off</option>
              <option value="user">user</option>
              <option value="project">project</option>
              <option value="local">local</option>
            </select>
          </FormField>
        )}
        {v.type === "subagent" && (v.memoryScope === "user" || v.memoryScope === "project") && (
          <FormField
            label="Seed memory"
            htmlFor="f-seed"
            hint="Governed block delivered into the subagent's MEMORY.md by orbeat-sync (role-visibility artifacts)"
          >
            <textarea
              id="f-seed"
              aria-label="Seed memory"
              className={`${inputCls} min-h-24 font-mono`}
              value={v.memorySeed}
              onChange={set("memorySeed")}
            />
          </FormField>
        )}
        {conflict && onReload ? (
          <ConflictNotice onReload={onReload} />
        ) : (
          error && <p className="text-sm text-danger">{error}</p>
        )}
        <Button variant="primary" className="justify-self-start" disabled={pending}>
          {submitLabel}
        </Button>
      </form>
    </Card>
  );
}

/**
 * Loads the full artifact by id before prefilling the edit form (C8 #2).
 * List rows are slim (Task 8: content/memorySeed omitted by default), and
 * store.UpdateArtifact is full-replace — prefilling from the list row and
 * saving would silently wipe an existing memorySeed the admin never saw.
 * One request per OPENED form, not per listed row: no N+1.
 */
function ArtifactEditForm({
  id,
  pending,
  error,
  conflict,
  onReload,
  onSubmit,
}: {
  id: string;
  pending: boolean;
  error: string;
  conflict: boolean;
  // Discards the in-progress edit and closes the form (Task 11 judgment
  // call — see ArtifactsPage's render site for the rationale). The by-id
  // query this component owns is refetched here, as the direct effect of
  // the click, before handing control back to the parent to close.
  onReload: () => void;
  // rowVersion is the by-id fetch's version, not the (possibly stale/slim)
  // list row's — see useUpdateArtifact's doc comment.
  onSubmit: (v: ArtifactInput, rowVersion: number) => void;
}) {
  const full = useAdminArtifact(id);
  // Data-first: once the by-id fetch has data at all, render the form from
  // it — even if `isError` is ALSO true, which happens when a background
  // revalidation (see useAdminArtifact's gcTime/refetchOnWindowFocus notes)
  // fails after the form is already showing real data. Checking `isError`
  // before `data` (the original order) unmounted a live, possibly
  // mid-edit form and swapped in an error panel on any such failure —
  // reproduced: type into the form, force a refetch that 500s, the typed
  // text was gone. `full.data` is only ever absent on the FIRST load.
  if (full.data) {
    const a = full.data;
    return (
      <ArtifactForm
        submitLabel="Save"
        pending={pending}
        error={error}
        conflict={conflict}
        onReload={() => {
          void full.refetch();
          onReload();
        }}
        roleGrants={a.roleGrants}
        // Computed from the SAVED artifact, not from the form's in-progress
        // state: the note reports what is being distributed right now, which
        // no amount of typing in this form changes until it is saved and
        // distributed.
        identityGap={pendingIdentity(a)}
        initial={{
          type: a.type,
          name: a.name,
          description: a.description,
          content: a.content,
          memoryScope: a.memoryScope ?? "",
          memorySeed: a.memorySeed ?? "",
          version: a.version,
          visibility: a.visibility,
        }}
        onSubmit={(v) => onSubmit(v, a.rowVersion)}
      />
    );
  }
  if (full.isError) {
    return (
      <div className="mt-4 flex items-center gap-3">
        <p className="text-sm text-danger">Failed to load artifact: {errMsg(full.error)}</p>
        <Button variant="ghost" onClick={() => void full.refetch()}>
          Retry
        </Button>
      </div>
    );
  }
  return <p className="mt-4 text-sm text-muted">Loading artifact…</p>;
}

function PublishBanner() {
  const { data } = useMarketplaceStatus();
  const publish = useMarketplacePublish();

  return (
    <div className="mb-6 flex items-center gap-4 rounded-xl border border-border bg-surface p-4 text-sm">
      <div className="flex-1">
        <span className="font-medium text-text">Marketplace publish status</span>
        {data?.lastCommit && (
          <span className="ml-2 font-mono text-xs text-faint">last commit: {data.lastCommit}</span>
        )}
        {!data?.lastCommit && <span className="ml-2 text-faint">no publish yet</span>}
        {data?.lastError && <span className="ml-2 text-xs text-danger">error: {data.lastError}</span>}
        {publish.error && (
          <span className="ml-2 text-xs text-danger">publish failed: {errMsg(publish.error)}</span>
        )}
      </div>
      <Button variant="ghost" onClick={() => publish.mutate()} disabled={publish.isPending}>
        {publish.isPending ? "Publishing…" : "Republish"}
      </Button>
    </div>
  );
}

function revisionDiffText(r: Pick<ArtifactRevision, "content" | "memorySeed">) {
  return r.memorySeed ? `${r.content}\n\n--- seed ---\n${r.memorySeed}` : r.content;
}

/**
 * Whether revision `num` is GONE, as opposed to merely not paged in yet.
 *
 * The panel's list is newest-first (internal/api's
 * handleListArtifactRevisions sorts revision_num DESC), so the query's NEXT
 * page holds OLDER revisions — `hasOlderPage` is `revs.hasNextPage` read
 * under that orientation. Revision numbers are contiguous (insertRevision
 * numbers MAX+1) and ORBEAT_ARTIFACT_REVISION_KEEP prunes a PREFIX of the
 * chain (docs/specs/2026-08-19-orbeat-revision-pruning-design.md §3), so the
 * loaded set is always a contiguous suffix: a number that is neither loaded
 * nor reachable by loading more was pruned and can never appear.
 *
 * This errs toward "may still arrive", which is the safe direction for both
 * callers. `hasOlderPage` can be true with the entire chain already on
 * screen, because the server's nextCursor is the `len(rows)==limit` "possibly
 * more" heuristic and a chain that is an exact multiple of the limit costs
 * one extra empty page.
 */
function isPruned(num: number, loaded: ArtifactRevision[], hasOlderPage: boolean) {
  return !hasOlderPage && !loaded.some((x) => x.revision === num);
}

// fable-audit §7 #16 item 2: `ArtifactRevision.content` was already in the
// payload (internal/api/admin_artifacts.go's artifactRevisionDTO carries it
// unconditionally, no slim projection) and was rendered nowhere. Each
// revision gets an on-demand line-level diff against its immediate
// predecessor (revision N-1), reusing the same shared Diff component as
// ReviewQueuePage rather than a second bespoke view.
//
// The diff is offered once the predecessor's FATE is known — three cases,
// two of which render the single undiffed pane:
//
//   - loaded                → real two-pane diff;
//   - never existed (rev 1)
//     or pruned away        → single undiffed pane;
//   - not paged in yet      → no affordance at all. Diffing against an empty
//                             string would falsely show the whole revision as
//                             "added"; "Load more revisions" brings the
//                             predecessor into view and the diff appears.
//
// The pruned case is why this is not a bare `prev !== undefined`. This
// comment previously stated the "Load more revisions" outcome as the contract
// for EVERY row with a missing predecessor, and revision pruning made that
// false: the cap removes a prefix, so the oldest survivor can be #9 with #8
// permanently absent, and the old `r.revision === 1 || prev !== undefined`
// gate never rendered that row's button — a revision the admin can see but
// can never inspect, with no explanation.
function RevisionItem({
  r,
  prev,
  prevPruned,
  restoredFromPruned,
  artifact,
  rollback,
  floor,
  pinningEnabled,
}: {
  r: ArtifactRevision;
  prev: ArtifactRevision | undefined;
  prevPruned: boolean;
  restoredFromPruned: boolean;
  artifact: AdminArtifact;
  rollback: ReturnType<typeof useRollbackArtifact>;
  // Shared with every other row in this panel, same as `rollback`: its
  // error/conflict state is rendered once at RevisionHistory level, not
  // per row, so this component only ever calls `.mutate` and reads
  // `.isPending`.
  floor: ReturnType<typeof useSetArtifactMinRevision>;
  // open-points.md point 6: a Community server does not register PUT
  // /v1/admin/artifacts/{id}/min-revision at all, so a row that offered
  // "Require this or newer" there would be clickable and 404. Gates that
  // button AND the "floor" pill below; see ArtifactsPage's own comment on
  // the table row's floor marker for why the pill is gated too, not only
  // the action.
  pinningEnabled: boolean;
}) {
  const [open, setOpen] = useState(false);
  // `r.revision === 1` is NOT subsumed by prevPruned: isPruned errs toward
  // "may still arrive", so the exact-multiple page heuristic can leave
  // hasOlderPage true with revision 1 already on screen — dropping this
  // clause would take the diff away from the row that has always had it.
  const canView = r.revision === 1 || prev !== undefined || prevPruned;
  const panelId = `revision-diff-${r.revision}`;
  // A pruned target is a pointer to a revision the admin cannot find in this
  // list. restored_from_num is a plain int column, not a foreign key, so the
  // reference outlives its row by design (spec §6); say so rather than render
  // it as a live pointer.
  const rollbackLabel = r.restoredFrom == null
    ? "rollback"
    : `rollback of #${r.restoredFrom}${restoredFromPruned ? " (pruned)" : ""}`;
  // The row that IS the current floor. `> 0` matters: 0 means "no floor",
  // never "floored at revision 0" (no revision numbered 0 exists), so
  // without it every row would compare false against an unset floor except
  // one that can never occur.
  const isFloor = artifact.minRevision > 0 && r.revision === artifact.minRevision;

  return (
    <li className="border-border py-3 [&+li]:border-t">
      <div className="grid grid-cols-[26px_1fr_auto] items-center gap-3">
        <span className="grid place-items-center">
          <span className={`h-3.5 w-3.5 rounded-full border-2 ${r.isCurrent ? "border-accent bg-accent" : "border-border-strong bg-surface"}`} />
        </span>
        <div>
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-sm font-semibold text-text">#{r.revision}</span>
            {r.source === "rollback"
              ? <Pill variant={restoredFromPruned ? "neutral" : "info"} dot={!restoredFromPruned}>{rollbackLabel}</Pill>
              : <Chip>approval</Chip>}
            {r.isCurrent && <Pill variant="accent">current</Pill>}
            {pinningEnabled && isFloor && <Pill variant="blue">floor</Pill>}
          </div>
          <div className="mt-0.5 text-xs text-faint">
            {r.source === "rollback" ? "restored" : "approved"} by <span className="text-muted">{r.approvedBy}</span> · <span className="font-mono">{new Date(r.approvedAt).toLocaleString()}</span>
          </div>
          {canView && (
            <button
              type="button"
              aria-expanded={open}
              aria-controls={panelId}
              onClick={() => setOpen((o) => !o)}
              className="mt-1.5 inline-flex items-center gap-1 rounded-md border border-border-strong px-2 py-1 text-xs font-medium text-muted hover:bg-surface-2 hover:text-text"
            >
              <span aria-hidden="true">{open ? "▾" : "▸"}</span>
              {open ? "Hide changes" : "View changes"}
            </button>
          )}
        </div>
        <div className="flex flex-col items-end gap-1.5">
          {!r.isCurrent && (
            <button
              // A revision is the complete approved state, so rolling back can
              // MOVE the artifact's file on every machine that receives it. The
              // confirmation names that before it fires, and names its absence
              // just as explicitly on a revision that recorded no identity: see
              // rollbackConfirmMessage.
              onClick={() => {
                if (window.confirm(rollbackConfirmMessage(r, distributedIdentity(artifact)))) {
                  rollback.mutate({ id: artifact.id, revision: r.revision });
                }
              }}
              disabled={rollback.isPending}
              className="rounded-md border border-border-strong px-2.5 py-1 text-xs font-semibold text-muted hover:border-accent hover:bg-accent-weak hover:text-accent-ink disabled:opacity-50"
            >
              Roll back to #{r.revision}
            </button>
          )}
          {/*
            Pre-this-slice, the isFloor branch here rendered a "Remove the
            floor" button. It is GONE, not moved: the artifact table row's
            own floor marker (ArtifactsPage's tableFloor control) is now the
            ONLY clear path, because it reads artifact.minRevision directly
            and works even when ORBEAT_ARTIFACT_REVISION_KEEP has pruned the
            floor's own revision out of THIS list, the exact gap that
            stranded this button (open-points.md's pinning row, point 7). A
            row-scoped control that sometimes has no row to scope to is
            strictly weaker than a table-scoped one that always has a row;
            keeping both would be two paths to one action with nothing
            distinguishing them (point 7's own framing). The floor's own row
            still carries the read-only "floor" pill above, gated on
            pinningEnabled the same as this button, so the panel still says
            which revision is held even though clearing it happens
            elsewhere.
          */}
          {pinningEnabled && !isFloor && (
            <button
              // The admin is looking at the revision she just approved, so the
              // action reads it straight off the row rather than asking her to
              // type a number (a free-text field is a way to typo a control on
              // a governance surface).
              onClick={() => {
                if (
                  window.confirm(
                    `Require revision #${r.revision} or newer for this artifact? Machines pinned below #${r.revision} will receive #${r.revision} on their next sync.`,
                  )
                ) {
                  floor.mutate({ id: artifact.id, minRevision: r.revision, rowVersion: artifact.rowVersion });
                }
              }}
              disabled={floor.isPending}
              className="rounded-md border border-border-strong px-2.5 py-1 text-xs font-semibold text-muted hover:border-accent hover:bg-accent-weak hover:text-accent-ink disabled:opacity-50"
            >
              Require this or newer
            </button>
          )}
        </div>
      </div>
      {open && canView && (
        <div id={panelId} className="mt-3">
          <Diff
            before={prev ? revisionDiffText(prev) : undefined}
            beforeLabel={prev ? `Revision #${prev.revision}` : undefined}
            after={revisionDiffText(r)}
            afterLabel={`Revision #${r.revision}`}
          />
        </div>
      )}
    </li>
  );
}

function RevisionHistory({
  artifact,
  onClose,
  onReload,
  pinningEnabled,
}: {
  artifact: AdminArtifact;
  onClose: () => void;
  // Refetches the artifacts list that `artifact` itself is drawn from
  // (ArtifactsPage's `historyArtifact`). Only used on a 412 from `floor`:
  // its own success already invalidates ["admin", "artifacts"], so the
  // fresh rowVersion the next attempt needs arrives on the next render for
  // free; a stale write needs the manual nudge, same as ReviewQueuePage's
  // ReviewCard onReload.
  onReload: () => void;
  // Threaded down to every RevisionItem; see that component's own comment.
  pinningEnabled: boolean;
}) {
  const revs = useArtifactRevisions(artifact.id);
  const rollback = useRollbackArtifact();
  const floor = useSetArtifactMinRevision();
  const floorConflict = isConflict(floor.error);
  const revisions = revs.rows;
  return (
    <div className="mt-4 max-w-2xl overflow-hidden rounded-xl border border-border bg-surface shadow-sm">
      <div className="flex items-center justify-between border-b border-border bg-inset px-4 py-3">
        <div className="flex items-center gap-2">
          <h2 className="text-sm font-semibold text-text">Version history</h2>
          <span className="font-mono text-xs text-faint">{revisions.length} approved versions</span>
        </div>
        <button onClick={onClose} className="text-xs text-faint hover:text-text">Close</button>
      </div>
      {rollback.error && <p className="px-4 pt-3 text-sm text-danger">{errMsg(rollback.error)}</p>}
      {floorConflict ? (
        <div className="px-4 pt-3">
          <ConflictNotice
            onReload={() => {
              floor.reset();
              onReload();
            }}
          />
        </div>
      ) : (
        floor.error && <p className="px-4 pt-3 text-sm text-danger">{errMsg(floor.error)}</p>
      )}
      {revisions.length === 0 && revs.isPending && <p className="px-4 py-3 text-sm text-faint">Loading…</p>}
      {revisions.length === 0 && revs.isError && (
        <p className="px-4 py-3 text-sm text-danger">Failed to load revisions: {errMsg(revs.error)}</p>
      )}
      {revs.isSuccess && revisions.length === 0 && (
        <p className="px-4 py-3 text-sm text-faint">No approved versions yet.</p>
      )}
      <ul className="px-4 py-1.5">
        {revisions.map((r) => (
          <RevisionItem
            key={r.revision}
            r={r}
            prev={revisions.find((x) => x.revision === r.revision - 1)}
            prevPruned={isPruned(r.revision - 1, revisions, revs.hasNextPage)}
            restoredFromPruned={r.restoredFrom != null && isPruned(r.restoredFrom, revisions, revs.hasNextPage)}
            artifact={artifact}
            rollback={rollback}
            floor={floor}
            pinningEnabled={pinningEnabled}
          />
        ))}
      </ul>
      {revisions.length > 0 && revs.isError && (
        // No separate Retry action: refetch() on an infinite query re-fetches
        // the last SUCCESSFUL page, not the failed next one. "Load more
        // revisions" below (fetchNextPage) is the control that correctly
        // retries the page that just failed.
        <p className="px-4 pb-2 text-sm text-danger">
          Failed to load more revisions: {errMsg(revs.error)}
        </p>
      )}
      {revs.isFetchingNextPage ? (
        <p className="px-4 pb-3 text-sm text-muted">Loading more…</p>
      ) : (
        revs.hasNextPage && (
          <div className="px-4 pb-3">
            {/* Named distinctly from the main list's "Load more": when the
                artifacts table AND this revision panel are both open, two
                controls named "Load more" would collide under Playwright's
                strict-mode getByRole (substring) lookup — see Task 10 review
                M2. */}
            <Button variant="ghost" onClick={() => void revs.fetchNextPage()}>
              Load more revisions
            </Button>
          </div>
        )
      )}
    </div>
  );
}

/**
 * The value a table cell's field is still being DISTRIBUTED under, when that
 * differs from the live value rendered above it. Renders nothing when there is
 * no gap, which is every row in a Community build and every unedited row in an
 * Enterprise one.
 */
function DistributedAs({ value }: { value: string | undefined }) {
  if (value === undefined) {
    return null;
  }
  return (
    <span role="note" className="mt-1 block text-xs font-normal text-warn">
      distributing as {value}
    </span>
  );
}

const findingSevCls: Record<ScanFinding["severity"], string> = {
  info: "text-muted bg-surface-2",
  warn: "text-warn bg-warn-weak",
  block: "text-danger bg-danger-weak",
};

/**
 * The author's own action on a pending submission the scanner had something
 * to say about (docs/plans/orbeat-scan-acknowledgment-2026-08-27.md). The
 * click gates APPROVAL, not submission -- the scanner runs inside the submit
 * transaction, so at the moment of submitting the findings do not exist yet
 * and there is nothing to acknowledge.
 *
 * Renders nothing unless ALL of: the artifact carries findings
 * (`scanFindingsDigest`, never recomputed client-side -- see
 * `AdminArtifact.findingsAcknowledged`'s own doc comment), it is still
 * pending, the viewer is the artifact's own submitter (POST
 * .../acknowledge-findings is submitter-only server-side, a 403 for anyone
 * else), and nobody has acknowledged the CURRENT digest yet.
 */
function FindingsAckPrompt({
  a,
  ack,
  subject,
}: {
  a: AdminArtifact;
  ack: ReturnType<typeof useAcknowledgeFindings>;
  subject: string;
}) {
  const digest = a.scanFindingsDigest;
  if (!digest || a.approvalState !== "pending" || a.submittedBy !== subject || a.findingsAcknowledged) {
    return null;
  }
  return (
    <div role="note" className="mt-1.5 max-w-sm rounded-md border border-warn bg-warn-weak px-2.5 py-2 text-xs text-text">
      <p className="font-medium">The scanner flagged this submission:</p>
      <ul className="mt-1 space-y-1">
        {(a.scanFindings ?? []).map((f, i) => (
          <li key={i} className={`flex flex-wrap items-center gap-1.5 rounded px-1.5 py-1 ${findingSevCls[f.severity]}`}>
            <span className="font-semibold uppercase">{f.severity}</span>
            <span className="font-mono">{f.rule}</span>
            <span>{f.message}</span>
          </li>
        ))}
      </ul>
      <button
        type="button"
        onClick={() => ack.mutate({ id: a.id, digest })}
        disabled={ack.isPending}
        aria-label={`Acknowledge findings for ${a.name}`}
        className="mt-1.5 rounded-md border border-border-strong px-2 py-1 text-xs font-semibold text-muted hover:border-accent hover:bg-accent-weak hover:text-accent-ink disabled:opacity-50"
      >
        Acknowledge findings
      </button>
    </div>
  );
}

export default function ArtifactsPage() {
  // order/q both reset paging to page one on ANY change (queries.ts's
  // useAdminList folds them into the query key), which is the load-bearing
  // part of Task 5: the API binds a cursor to the sort/direction it was minted under
  // and 400s a replay under a different one. The sortable column is Type
  // (artifactSortName = "type"), NOT Name -- the list's own second sort key
  // -- but ?q= searches name regardless (internal/api/admin_artifacts.go's
  // own comment on handleListArtifacts explains why the two deliberately
  // differ here, unlike every other searchable list).
  const [order, setOrder] = useState<"asc" | "desc">("asc");
  // `q` is the box's instant display value; the query itself is driven by
  // the debounced value below (B35: this used to fire one request per
  // keystroke — ListSearchBox's onChange fed the query key directly).
  const [q, setQ] = useState("");
  const debouncedQ = useDebouncedValue(q, SEARCH_DEBOUNCE_MS);
  const artifactsQuery = useAdminArtifacts({ order, q: debouncedQ });
  const { rows: artifacts } = artifactsQuery;
  const create = useCreateArtifact();
  const update = useUpdateArtifact();
  const del = useDeleteArtifact();
  const submit = useSubmitArtifact();
  const withdraw = useWithdrawArtifact();
  // The author's own acknowledgment of scan findings on a pending submission
  // (docs/plans/orbeat-scan-acknowledgment-2026-08-27.md). One shared
  // instance for every row's FindingsAckPrompt, the same convention `submit`
  // and `withdraw` above already use.
  const ack = useAcknowledgeFindings();
  // A digest mismatch (the artifact was re-scanned since this row was
  // loaded) comes back as a 412, exactly like a stale row_version -- both
  // mean "reload to see the current state", so this reuses isConflict/
  // ConflictNotice rather than a second bespoke notice.
  const ackConflict = isConflict(ack.error);
  const { subject } = useAuth();
  // open-points.md point 6: the portal's only edition signal. Fail-closed
  // while GET /v1/me is loading or has errored; see
  // useArtifactPinningEnabled's own comment for why that direction and not
  // the other.
  const pinningEnabled = useArtifactPinningEnabled();
  // The table row's own floor-clear control (point 7). A SEPARATE
  // useSetArtifactMinRevision instance from RevisionHistory's `floor`: the
  // two render in different places (this one needs no revision panel open
  // at all) and each needs its own isPending/error, the same reason every
  // other mutation on this page (create, update, del, submit, withdraw) is
  // its own hook call rather than a shared one threaded everywhere.
  const tableFloor = useSetArtifactMinRevision();
  const tableFloorConflict = isConflict(tableFloor.error);
  const [mode, setMode] = useState<"closed" | "create" | AdminArtifact>("closed");
  const [historyFor, setHistoryFor] = useState<string | null>(null);
  // The panel needs the artifact, not just its id: the rollback confirmation
  // has to name the identity being distributed today to say what a rollback
  // moves. Looked up in the list on every render rather than captured in
  // state at click time, so a rollback (which invalidates ["admin",
  // "artifacts"]) refreshes what the NEXT confirmation claims.
  const historyArtifact = artifacts.find((a) => a.id === historyFor);

  // Reset the mutation when (re)opening a form so a prior failed submit's error
  // never bleeds into a freshly opened form.
  const openCreate = () => {
    create.reset();
    setMode("create");
  };
  const openEdit = (a: AdminArtifact) => {
    update.reset();
    setMode(a);
  };

  const artifactsTable = (
    <Panel className="mt-4">
      <table className="w-full text-sm">
        <thead className="bg-inset">
          <tr>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Name</th>
            <SortableTh label="Type" order={order} onToggle={() => setOrder(order === "asc" ? "desc" : "asc")} />
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">State</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Visibility</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {artifacts.map((a, i) => {
            const last = i === artifacts.length - 1;
            const cellCls = `p-3 ${last ? "" : "border-b border-border"}`;
            // Per-cell rather than one marker on the row: a single note in the
            // Name column would silently omit a type or visibility change, and
            // those move the file and switch the channel just as a rename
            // does. Each marker sits under the value it contradicts.
            const gap = pendingIdentity(a);
            const distributedAs = (f: IdentityField) => gap.find((c) => c.field === f)?.from;
            return (
              <tr key={a.id} className={`hover:bg-inset ${historyFor === a.id ? "bg-accent-weak" : ""}`}>
                <td className={`${cellCls} font-medium text-text`}>
                  {a.name}
                  <DistributedAs value={distributedAs("name")} />
                </td>
                <td className={cellCls}>
                  <Chip variant={a.type}>{a.type}</Chip>
                  <DistributedAs value={distributedAs("type")} />
                </td>
                <td className={cellCls}>
                  <StateBadge state={a.approvalState} />
                  {/* Visible without opening Version history: the admin
                      override on every client-side pin is otherwise invisible
                      from the main table. 0 means no floor and renders
                      nothing, same convention as the "current floor" marker
                      on the matching RevisionItem row.

                      Gated on pinningEnabled, same as the panel's own floor
                      UI: a Community server can never have set minRevision >
                      0 in practice (the route that sets it is not registered
                      there), but hiding the whole block rather than leaving a
                      read-only number with no way to act on it keeps a
                      downgraded/stale edition from showing a trace of a
                      feature this console cannot manage.

                      Clearing reads artifact.minRevision directly off this
                      row, which is exactly why capo chose the table row over
                      the revision panel for the clear control (open-points.md
                      point 7): it is correct even when
                      ORBEAT_ARTIFACT_REVISION_KEEP has pruned the floor's own
                      revision out of the panel's list, where the OLD
                      "Remove the floor" button had no row left to render on. */}
                  {pinningEnabled && a.minRevision > 0 && (
                    <span className="mt-1 flex items-center gap-1.5">
                      <Pill variant="blue">floor #{a.minRevision}</Pill>
                      <button
                        type="button"
                        onClick={() => {
                          if (
                            window.confirm(
                              `Clear the minimum-revision floor on ${a.name}? Machines pinned below the current revision are no longer held once they next sync.`,
                            )
                          ) {
                            tableFloor.mutate({ id: a.id, minRevision: 0, rowVersion: a.rowVersion });
                          }
                        }}
                        aria-label={`Clear floor for ${a.name}`}
                        disabled={tableFloor.isPending}
                        className="text-xs font-medium text-muted hover:text-danger disabled:opacity-50"
                      >
                        Clear
                      </button>
                    </span>
                  )}
                  <FindingsAckPrompt a={a} ack={ack} subject={subject} />
                </td>
                <td className={cellCls}>
                  <Chip>{a.visibility}</Chip>
                  <DistributedAs value={distributedAs("visibility")} />
                </td>
                <td className={`${cellCls} text-right`}>
                  <div className="inline-flex items-center gap-3">
                    {(a.approvalState === "draft" || a.approvalState === "rejected") && (
                      <button onClick={() => submit.mutate(a.id)} aria-label={`Submit ${a.name}`} className="text-sm font-medium text-muted hover:text-text">
                        Submit
                      </button>
                    )}
                    {a.approved && (
                      <button onClick={() => withdraw.mutate(a.id)} aria-label={`Withdraw ${a.name}`} className="text-sm font-medium text-muted hover:text-text">
                        Withdraw
                      </button>
                    )}
                    <button onClick={() => setHistoryFor(a.id)} aria-label={`History for ${a.name}`} className="text-sm font-medium text-muted hover:text-text">
                      History
                    </button>
                    <button onClick={() => openEdit(a)} aria-label={`Edit ${a.name}`} className="text-sm font-medium text-muted hover:text-text">
                      Edit
                    </button>
                    <button
                      onClick={() => {
                        if (window.confirm(`Delete ${a.name}?`)) del.mutate(a.id);
                      }}
                      aria-label={`Delete ${a.name}`}
                      className="text-sm font-medium text-danger hover:text-danger"
                    >
                      Delete
                    </button>
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </Panel>
  );

  return (
    <div>
      <PublishBanner />

      <div className="flex items-center justify-between">
        <div>
          <p className="text-xs font-semibold uppercase tracking-wide text-faint">Governed catalog</p>
          <h1 className="mt-1 text-2xl font-semibold text-text">Artifacts</h1>
        </div>
        <div className="flex items-center gap-3">
          <ListSearchBox value={q} onChange={setQ} label="Search artifacts" />
          <Button variant="primary" onClick={openCreate}>
            New artifact
          </Button>
        </div>
      </div>

      {(submit.error || withdraw.error) && (
        <p className="mt-2 text-sm text-danger">{errMsg(submit.error ?? withdraw.error)}</p>
      )}
      {del.error && (
        <p className="mt-2 text-sm text-danger">Delete failed: {errMsg(del.error)}</p>
      )}
      {tableFloorConflict ? (
        <div className="mt-2">
          <ConflictNotice
            onReload={() => {
              tableFloor.reset();
              void artifactsQuery.refetch();
            }}
          />
        </div>
      ) : (
        tableFloor.error && (
          <p className="mt-2 text-sm text-danger">Floor change failed: {errMsg(tableFloor.error)}</p>
        )
      )}
      {ackConflict ? (
        <div className="mt-2">
          <ConflictNotice
            onReload={() => {
              ack.reset();
              void artifactsQuery.refetch();
            }}
          />
        </div>
      ) : (
        ack.error && (
          <p className="mt-2 text-sm text-danger">Acknowledge failed: {errMsg(ack.error)}</p>
        )
      )}

      {artifacts.length === 0 ? (
        // The gate owns loading/error for the first page; once any rows are
        // loaded, later pages are handled inline below so the accumulated
        // table never unmounts (see queries.ts's useAdminList doc comment).
        <QueryGate query={artifactsQuery} label="artifacts">
          {() => artifactsTable}
        </QueryGate>
      ) : (
        <>
          {artifactsTable}
          {artifactsQuery.isError && (
            // No separate Retry action: refetch() on an infinite query
            // re-fetches the last SUCCESSFUL page, not the failed next one.
            // "Load more" below (fetchNextPage) is the control that
            // correctly retries the page that just failed.
            <p className="mt-2 text-sm text-danger">
              Failed to load more artifacts: {errMsg(artifactsQuery.error)}
            </p>
          )}
          {artifactsQuery.isFetchingNextPage ? (
            <p className="mt-2 text-sm text-muted">Loading more…</p>
          ) : (
            artifactsQuery.hasNextPage && (
              <Button variant="ghost" className="mt-2" onClick={() => void artifactsQuery.fetchNextPage()}>
                Load more
              </Button>
            )
          )}
        </>
      )}

      {mode === "create" && (
        <ArtifactForm
          initial={EMPTY}
          submitLabel="Create"
          pending={create.isPending}
          error={errMsg(create.error)}
          onSubmit={(v) => create.mutate(v, { onSuccess: () => setMode("closed") })}
        />
      )}

      {mode !== "closed" && mode !== "create" && (
        <ArtifactEditForm
          key={mode.id}
          id={mode.id}
          pending={update.isPending}
          error={errMsg(update.error)}
          conflict={isConflict(update.error)}
          // Reload discards the in-progress edit (spec §10 / Task 11
          // judgment call) — see the Task 11 report for the
          // preserve-vs-discard tradeoff.
          onReload={() => {
            update.reset();
            setMode("closed");
          }}
          onSubmit={(v, rowVersion) =>
            update.mutate(
              { id: mode.id, input: v, rowVersion },
              { onSuccess: () => setMode("closed") },
            )
          }
        />
      )}

      {historyArtifact && (
        <RevisionHistory
          key={historyArtifact.id}
          artifact={historyArtifact}
          onClose={() => setHistoryFor(null)}
          onReload={() => void artifactsQuery.refetch()}
          pinningEnabled={pinningEnabled}
        />
      )}
    </div>
  );
}
