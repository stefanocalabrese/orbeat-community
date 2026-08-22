import { useState, type ChangeEvent } from "react";
import {
  useAdminArtifact,
  useAdminArtifacts,
  useArtifactRevisions,
  useCreateArtifact,
  useDeleteArtifact,
  useMarketplacePublish,
  useMarketplaceStatus,
  useRollbackArtifact,
  useUpdateArtifact,
  useSubmitArtifact,
  useWithdrawArtifact,
} from "../../api/queries";
import type { AdminArtifact, ArtifactInput, ArtifactRevision, ArtifactRoleGrants } from "../../api/types";
import { errMsg } from "../../api/client";
import { FormField, inputCls } from "../../components/FormField";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { StateBadge, Pill, Chip } from "../../components/ui/Badge";
import { Card, Panel } from "../../components/ui/Card";
import { Diff } from "../../components/ui/Diff";
import { ConflictNotice } from "./ConflictNotice";
import { isConflict } from "./conflict";

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
      immediately. Remove them on the Artifact entitlements page first if that is not what you want.
    </p>
  );
}

function ArtifactForm({ initial, onSubmit, pending, error, conflict = false, onReload, submitLabel, roleGrants }: ArtifactFormProps) {
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
  id,
  rollback,
}: {
  r: ArtifactRevision;
  prev: ArtifactRevision | undefined;
  prevPruned: boolean;
  restoredFromPruned: boolean;
  id: string;
  rollback: ReturnType<typeof useRollbackArtifact>;
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
        {!r.isCurrent && (
          <button
            onClick={() => { if (window.confirm(`Roll distribution back to revision #${r.revision}?`)) rollback.mutate({ id, revision: r.revision }); }}
            disabled={rollback.isPending}
            className="rounded-md border border-border-strong px-2.5 py-1 text-xs font-semibold text-muted hover:border-accent hover:bg-accent-weak hover:text-accent-ink disabled:opacity-50"
          >
            Roll back to #{r.revision}
          </button>
        )}
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

function RevisionHistory({ id, onClose }: { id: string; onClose: () => void }) {
  const revs = useArtifactRevisions(id);
  const rollback = useRollbackArtifact();
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
            id={id}
            rollback={rollback}
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

export default function ArtifactsPage() {
  const artifactsQuery = useAdminArtifacts();
  const { rows: artifacts } = artifactsQuery;
  const create = useCreateArtifact();
  const update = useUpdateArtifact();
  const del = useDeleteArtifact();
  const submit = useSubmitArtifact();
  const withdraw = useWithdrawArtifact();
  const [mode, setMode] = useState<"closed" | "create" | AdminArtifact>("closed");
  const [historyFor, setHistoryFor] = useState<string | null>(null);

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
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Type</th>
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
            return (
              <tr key={a.id} className={`hover:bg-inset ${historyFor === a.id ? "bg-accent-weak" : ""}`}>
                <td className={`${cellCls} font-medium text-text`}>{a.name}</td>
                <td className={cellCls}>
                  <Chip variant={a.type}>{a.type}</Chip>
                </td>
                <td className={cellCls}>
                  <StateBadge state={a.approvalState} />
                </td>
                <td className={cellCls}>
                  <Chip>{a.visibility}</Chip>
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
        <Button variant="primary" onClick={openCreate}>
          New artifact
        </Button>
      </div>

      {(submit.error || withdraw.error) && (
        <p className="mt-2 text-sm text-danger">{errMsg(submit.error ?? withdraw.error)}</p>
      )}
      {del.error && (
        <p className="mt-2 text-sm text-danger">Delete failed: {errMsg(del.error)}</p>
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

      {historyFor && <RevisionHistory key={historyFor} id={historyFor} onClose={() => setHistoryFor(null)} />}
    </div>
  );
}
