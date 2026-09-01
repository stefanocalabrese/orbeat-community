import { keepPreviousData, useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useToast } from "../components/ui/toastContext";
import { useAuth } from "../auth/useAuth";
import { apiFetch } from "./client";
import type {
  AdminArtifact,
  AdminServer,
  ArtifactEntitlement,
  ArtifactInput,
  ArtifactRevision,
  AuditPage,
  CatalogServer,
  Entitlement,
  Me,
  Page,
  PublishStatus,
  Role,
  RoleDeleteResult,
  ServerInput,
  ServerUpdateInput,
  VirtualKey,
  VirtualKeyInput,
} from "./types";

/**
 * ?order and ?q for an admin list (docs/plans/orbeat-admin-search-sort-
 * 2026-08-27.md Tasks 3-4). No ?sort field: every list allows exactly one
 * sort column today (internal/api/paging.go's allowlist), already that
 * list's existing default, so the only axis a client actually controls is
 * direction. q is split into its own type, ListSearchParams, rather than
 * living on every list's params: entitlements and artifact-entitlements
 * REFUSE ?q= with 400 the instant the parameter is PRESENT at all
 * (internal/api/paging.go's refuseSearch keys on Query().Has("q"), not on
 * the value), so their hooks (useEntitlements/useArtifactEntitlements) take
 * ListOrderParams, which has no q field: a caller for those two lists has
 * no way to pass one, not merely a convention not to.
 */
export type ListOrderParams = { order?: "asc" | "desc" };
export type ListSearchParams = ListOrderParams & { q?: string };

/**
 * Cursor-paginated admin list: fetches page 1, exposes `rows` as the flat
 * accumulation of every page fetched so far (never just the current page),
 * plus `fetchNextPage`/`hasNextPage`/`isFetchingNextPage` for a "Load more"
 * control. Built on `useInfiniteQuery` rather than component-local
 * accumulator state (contrast AuditPage, which keeps its own cursor+rows
 * `useState`) for one concrete reason: every one of these lists has
 * create/update/delete mutations that invalidate its query key. A
 * component-local accumulator only knows how to APPEND a new page — it has
 * no way to re-merge a stale earlier page when the underlying list is
 * invalidated out from under it, so e.g. a role created after "Load more"
 * had already been clicked once would silently never appear.
 * `useInfiniteQuery` refetches every already-loaded page on invalidation and
 * rebuilds `data.pages` from scratch, so `rows` is always the accumulation of
 * CURRENT pages, never stale ones.
 *
 * Deliberately not unified with AuditPage's pagination (different cursor
 * encoding — opaque JSON-array vs "a:b" — different limits — 100/500 vs
 * 50/1000 — and audit has no mutations to invalidate it).
 *
 * The rest of `query`'s shape (isPending/isError/error/refetch/…) is passed
 * through unchanged, so it plugs directly into `<QueryGate query={...}>` —
 * verified against the real UseQueryResult<T> generic, not assumed.
 *
 * No `?limit` is ever sent — the server's clamped default (100, see
 * internal/api/paging.go's defaultListLimit) applies. `limit` below is read
 * from the FIRST page's own envelope (rather than left as a parsed-but-unused
 * field) so callers can render an accurate "first N" disclosure without a
 * second hardcoded copy of that number that could drift from the server's.
 *
 * `params` (order/q, docs/plans/orbeat-admin-search-sort-2026-08-27.md Task
 * 5) is folded into `queryKey`, not only into the request URL. THIS is what
 * drops an outstanding cursor on a sort or search change, which the plan
 * calls the load-bearing part of this feature. A distinct (order, q) pair is
 * a distinct queryKey, so react-query starts that key fresh at
 * `initialPageParam` ("") instead of resuming the PREVIOUS key's accumulated
 * pages and their cursor. The API binds a cursor to the sort/direction it
 * was minted under and 400s on a mismatch (8935bb9, 8e0636c). Folding
 * params into the URL but leaving them OUT of the queryKey is the mutant
 * this slice's own test (queries.sortsearch.test.tsx) proves against: the
 * list would still render correctly on first render, but changing order or
 * q would leave the OLD key's cached pages in place, and the next "Load
 * more" would replay a cursor minted under the old sort against the new
 * one's URL, the exact 400 this whole slice exists to prevent.
 */
function useAdminList<K extends string, Row>(
  queryKey: readonly unknown[],
  path: string,
  rowsKey: K,
  enabled = true,
  params: ListSearchParams = {},
) {
  const { token } = useAuth();
  const extra = new URLSearchParams();
  if (params.order === "desc") extra.set("order", "desc");
  if (params.q) extra.set("q", params.q);
  const extraQS = extra.toString();
  const basePath = extraQS ? `${path}${path.includes("?") ? "&" : "?"}${extraQS}` : path;
  const query = useInfiniteQuery({
    queryKey: [...queryKey, params.order ?? "asc", params.q ?? ""],
    queryFn: ({ pageParam, signal }) =>
      apiFetch<Page<K, Row>>(
        // `basePath` may already carry its own query string (e.g. the review
        // queue's `?state=pending&include=content`, or order/q above), so the
        // cursor param must join with `&` in that case rather than a second
        // `?`.
        pageParam
          ? `${basePath}${basePath.includes("?") ? "&" : "?"}cursor=${encodeURIComponent(pageParam)}`
          : basePath,
        token,
        { signal },
      ),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextCursor || undefined,
    enabled,
    // B35: without this, changing order/q (both folded into queryKey above)
    // makes `query.data` briefly go back to undefined while the new key's
    // first page is in flight — `rows` collapses to `[]` for that window.
    // ArtifactsPage derives its Version-history panel's target artifact by
    // looking it up in `rows` (`artifacts.find(a => a.id === historyFor)`),
    // so that collapse silently unmounted an open panel on every sort or
    // search change, even when the artifact it was showing is still in the
    // page that is about to arrive. keepPreviousData keeps the PRIOR key's
    // rows on screen until the new key's data actually lands, so a page
    // that still contains the same row never has a reason to unmount
    // anything depending on it.
    placeholderData: keepPreviousData,
  });
  const rows = query.data?.pages.flatMap((p) => p[rowsKey]) ?? [];
  const limit = query.data?.pages[0]?.limit;
  return { ...query, rows, limit };
}

/** GET /v1/me: the caller's identity plus edition-dependent `features`. */
export function useMe() {
  const { token } = useAuth();
  return useQuery({
    queryKey: ["me"],
    queryFn: ({ signal }) => apiFetch<Me>("/v1/me", token, { signal }),
  });
}

/**
 * Whether the artifact minimum-revision floor controls (the table row's
 * clear affordance and the revision panel's "Require this or newer") should
 * render at all.
 *
 * Fail-closed by construction: `=== true` is the only path to a true
 * result, so a still-loading query, a failed one, and an explicit `false`
 * all resolve to hidden. That is a deliberate choice between two loading
 * states, not an accident of how the boolean happens to read. The
 * alternative, showing the controls optimistically and removing them once
 * Community is confirmed, flashes a paid control into a Community admin's
 * screen, momentarily clickable, on every page load. Hiding a control an
 * Enterprise admin is entitled to for the same brief window is the smaller
 * failure, and it is the SAME shape every other loading state on this page
 * already uses (e.g. ArtifactEditForm's "Loading artifact…", QueryGate's
 * default of rendering nothing until data arrives) rather than a new pattern
 * introduced just for this control.
 *
 * `features?.pinning` (not `features.pinning`): the same forward-compat
 * degradation GET /v1/sync/config's own `pinning` field documents applies
 * here too. A server predating this field returns a body with no
 * `features` key at all, decodes as `undefined`, and must resolve to
 * hidden rather than throw.
 */
export function useArtifactPinningEnabled(): boolean {
  return useMe().data?.features?.pinning === true;
}

/**
 * Whether VirtualKeysPage should render at all.
 *
 * `useArtifactPinningEnabled` above answers the same question for a
 * HANDFUL OF CONTROLS inside an existing page, its still-loading and
 * explicit-`false` cases both collapse to hidden, and this hook collapses
 * them the same way, for the same fail-closed reason (see that hook's own
 * comment). What differs here is the blast radius of getting it wrong: the
 * whole console page a Community admin would otherwise briefly see is one
 * whose every action, list, create, revoke, hits a route
 * (POST/GET/DELETE /v1/admin/virtual-keys) that does not exist on that
 * server at all, not a handful of buttons on an otherwise-working page.
 * `=== true` is still the only path to a true result, so a still-loading
 * `useMe()` query, a failed one, and an explicit `false` all resolve to
 * "render nothing".
 *
 * `features?.virtualKeys` (not `features.virtualKeys`): the same
 * forward-compat degradation `features?.pinning` documents applies here.
 * A server predating this field returns a body with no `features` key at
 * all, decodes as `undefined`, and must resolve to hidden rather than throw.
 */
export function useVirtualKeysEnabled(): boolean {
  return useMe().data?.features?.virtualKeys === true;
}

export function useCatalog() {
  const { token } = useAuth();
  return useQuery({
    queryKey: ["catalog"],
    queryFn: ({ signal }) =>
      apiFetch<{ servers: CatalogServer[] }>("/v1/catalog", token, { signal }),
  });
}

export function useAdminServers(params: ListSearchParams = {}) {
  return useAdminList<"servers", AdminServer>(["admin", "servers"], "/v1/admin/servers", "servers", true, params);
}

export function useRoles(params: ListSearchParams = {}) {
  return useAdminList<"roles", Role>(["admin", "roles"], "/v1/admin/roles", "roles", true, params);
}

/**
 * order-only (ListOrderParams, not ListSearchParams): entitlements REFUSE
 * ?q= with 400 (see ListOrderParams's own comment above), so this hook's
 * params type has no q field for a caller to pass in the first place.
 */
export function useEntitlements(params: ListOrderParams = {}) {
  return useAdminList<"entitlements", Entitlement>(
    ["admin", "entitlements"],
    "/v1/admin/entitlements",
    "entitlements",
    true,
    params,
  );
}

/**
 * AuditFilters narrows the audit list server-side. An empty string means "no
 * narrowing", matching the API, where an absent and an empty parameter are the
 * same thing.
 */
export type AuditFilters = { actor: string; action: string; decision: string };

export const emptyAuditFilters: AuditFilters = { actor: "", action: "", decision: "" };

export function useAuditPage(cursor: string, filters: AuditFilters = emptyAuditFilters) {
  const { token } = useAuth();
  const params = new URLSearchParams({ limit: "50" });
  if (cursor) params.set("cursor", cursor);
  // Ordered actor, action, decision so the query string is stable for a given
  // filter set: the queryKey below is what react-query caches on, and a URL
  // that varied by insertion order would fetch the same page twice.
  if (filters.actor) params.set("actor", filters.actor);
  if (filters.action) params.set("action", filters.action);
  if (filters.decision) params.set("decision", filters.decision);
  return useQuery({
    queryKey: ["admin", "audit", cursor, filters.actor, filters.action, filters.decision],
    queryFn: ({ signal }) => apiFetch<AuditPage>(`/v1/admin/audit?${params.toString()}`, token, { signal }),
  });
}

// TResult defaults to unknown so every pre-existing caller (which never
// reads a mutation's success value — see e.g. useDeleteServer) is unaffected;
// it is inferred from `fn`'s actual return type only where a caller cares,
// as useDeleteRole below does to surface DELETE /v1/admin/roles/{id}'s
// revoked-grant counts (spec §7).
function useInvalidating<TArgs, TResult = unknown>(
  fn: (token: string, a: TArgs) => Promise<TResult>,
  keys: string[][],
  /**
   * What to tell the user when this mutation succeeds. Required rather than
   * defaulted to something like "Saved": a uniform message on twenty different
   * actions is noise a user learns to ignore, and the point of the toast is
   * that the thing they just did is the thing that happened.
   *
   * FAILURE is deliberately not toasted here. Every admin page already renders
   * its own inline error, several of them with an action attached (a 412 tells
   * you to reload, a 402 opens the cap dialog), and a message that
   * auto-dismisses is the wrong place for something you have to act on.
   */
  successMessage: string,
) {
  const { token } = useAuth();
  const qc = useQueryClient();
  const { push } = useToast();
  return useMutation({
    mutationFn: (a: TArgs) => fn(token, a),
    onSuccess: () => {
      keys.forEach((k) => void qc.invalidateQueries({ queryKey: k }));
      push(successMessage);
    },
  });
}

export const useCreateServer = () =>
  useInvalidating(
    (t, input: ServerInput) =>
      apiFetch("/v1/admin/servers", t, { method: "POST", body: input }),
    [["admin", "servers"], ["catalog"]],
    "MCP server created.",
  );

export const useUpdateServer = () =>
  useInvalidating(
    // rowVersion comes from the list row being edited (ServersPage prefills
    // its form from that row, and the list carries a real, non-zero version —
    // see AdminServer.rowVersion). Quoted to match the server's strong ETag;
    // an unquoted If-Match is a 400.
    //
    // input is ServerUpdateInput, not ServerInput (defect 1, 2026-09-01,
    // BREAKING): secretRef/tlsCaRef are `?: string` there, so ServersPage's
    // toServerUpdateInput can leave one `undefined` to omit it from the PUT
    // body entirely — see that type's own doc comment for the wire contract.
    (t, a: { id: string; input: ServerUpdateInput; rowVersion: number }) =>
      apiFetch(`/v1/admin/servers/${a.id}`, t, {
        method: "PUT",
        body: a.input,
        ifMatch: `"${a.rowVersion}"`,
      }),
    [["admin", "servers"], ["catalog"]],
    "MCP server updated.",
  );

/**
 * Editing a grant's allowed tools, which before this needed delete-and-recreate.
 *
 * rowVersion comes from the list row being edited, like useUpdateServer: the
 * entitlement list carries a real, non-zero version. Quoted to match the
 * server's strong ETag; an unquoted If-Match is a 400.
 *
 * roleId and mcpServerId are deliberately NOT sent. The server ignores them,
 * and sending them would suggest to the next reader that a grant can be
 * repointed by an edit, which is precisely what the API refuses to do.
 */
export const useUpdateEntitlement = () =>
  useInvalidating(
    (t, a: { id: string; allowedTools: string[] | null; permissions: string[]; rowVersion: number }) =>
      apiFetch(`/v1/admin/entitlements/${a.id}`, t, {
        method: "PUT",
        body: { allowedTools: a.allowedTools, permissions: a.permissions },
        ifMatch: `"${a.rowVersion}"`,
      }),
    [["admin", "entitlements"], ["catalog"]],
    "Allowed tools updated.",
  );

export const useDeleteServer = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/servers/${id}`, t, { method: "DELETE" }),
    [["admin", "servers"], ["catalog"]],
    "MCP server deleted.",
  );

/**
 * Deleting a role cascades server AND artifact entitlements (spec §3), so
 * ["admin","roles"] alone is NOT enough here — the Entitlements and
 * Artifact-Entitlements admin pages would keep showing grants that the
 * DELETE just revoked server-side. All three keys are invalidated together;
 * do not "simplify" this back down to just the roles key.
 */
export const useDeleteRole = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch<RoleDeleteResult>(`/v1/admin/roles/${id}`, t, { method: "DELETE" }),
    [["admin", "roles"], ["admin", "entitlements"], ["admin", "artifactEntitlements"]],
    "Role deleted.",
  );

export function useAdminArtifacts(params: ListSearchParams = {}) {
  return useAdminList<"artifacts", AdminArtifact>(
    ["admin", "artifacts"],
    "/v1/admin/artifacts",
    "artifacts",
    true,
    params,
  );
}

/**
 * Fetches one artifact by id, which ALWAYS carries the full payload —
 * GET /v1/admin/artifacts/{id} is not subject to Task 8's slim-by-default
 * list projection (internal/api/admin_artifacts.go's handleGetArtifact).
 * This is the fix for C8 #2: the edit form must never prefill from a slim
 * list row, because store.UpdateArtifact is full-replace — saving a form
 * primed with the list row's blank content/memorySeed would silently wipe
 * an existing memorySeed the admin never saw (TestArtifactUpdateMemorySeedFullReplaceClears
 * pins that semantic server-side). One request per opened form, not per
 * listed row, so this is not an N+1.
 *
 * Task 11 review, Important #2: `gcTime: 0`. This query is observed only by
 * the currently-open edit form (the queryKey is per-id), so once that form
 * closes there is nothing left with a reason to keep its data cached.
 * Without this, opening artifact A then B then A again served A's SECOND
 * open from the first fetch's leftover cache entry (isPending false,
 * instantly) while a background revalidation ran behind it — and because
 * ArtifactForm's `useState(initial)` only reads its `initial` prop once on
 * mount, that revalidation's fresh data was silently dropped even after it
 * resolved. Reproduced: edit A, edit B, change A server-side, reopen A —
 * the field kept showing the pre-change value forever. `gcTime: 0` forces
 * every reopen through "Loading artifact…" and a real fetch, so the form's
 * one-time `initial` read is always of current data.
 */
export function useAdminArtifact(id: string) {
  const { token } = useAuth();
  return useQuery({
    queryKey: ["admin", "artifacts", "byId", id],
    queryFn: ({ signal }) => apiFetch<AdminArtifact>(`/v1/admin/artifacts/${id}`, token, { signal }),
    // The wrapper (ArtifactEditForm) only ever mounts with a real id, so
    // this never actually gates anything — kept for parity with
    // useArtifactRevisions' convention, not as a guard against a falsy id.
    enabled: !!id,
    gcTime: 0,
    // Task 11 review, Important #1 (defense in depth — see ArtifactEditForm
    // for the primary fix): a background refetch of a form the admin is
    // actively editing has no upside, and every trigger left enabled is one
    // more chance to regress the same class of bug.
    refetchOnWindowFocus: false,
  });
}

export const useCreateArtifact = () =>
  useInvalidating(
    (t, input: ArtifactInput) =>
      apiFetch("/v1/admin/artifacts", t, { method: "POST", body: input }),
    [["admin", "artifacts"], ["catalog"]],
    "Artifact created.",
  );

export const useUpdateArtifact = () =>
  useInvalidating(
    // rowVersion comes from useAdminArtifact's by-id fetch (ArtifactEditForm),
    // not the slim list row — the same reason the form's content/memorySeed
    // are prefilled from that fetch (see useAdminArtifact's doc comment):
    // the list row can be stale/slim, and the by-id fetch is what the admin
    // actually reviewed before editing. Quoted to match the server's strong
    // ETag; an unquoted If-Match is a 400.
    (t, a: { id: string; input: ArtifactInput; rowVersion: number }) =>
      apiFetch(`/v1/admin/artifacts/${a.id}`, t, {
        method: "PUT",
        body: a.input,
        ifMatch: `"${a.rowVersion}"`,
      }),
    [["admin", "artifacts"], ["catalog"]],
    "Artifact updated.",
  );

export const useDeleteArtifact = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/artifacts/${id}`, t, { method: "DELETE" }),
    [["admin", "artifacts"], ["catalog"]],
    "Artifact deleted.",
  );

export function useReviewQueue() {
  // Built on the shared useAdminList infinite-query helper (Task 10), not a
  // one-off useQuery, for two reasons found in C8/Task 11:
  //
  // 1. Task 8 moved ?state into SQL and made GET /v1/admin/artifacts
  //    keyset-paginated with a 100-row default cap. A plain useQuery here
  //    (the pre-Task-10 shape) SILENTLY TRUNCATES at 100 pending artifacts —
  //    worse than the pre-Task-8 unpaginated behavior, since it now truncates
  //    rather than returning everything. useAdminList gives the queue the
  //    same "Load more" append semantics as the other five lists.
  // 2. include=content is required: the queue renders a per-card
  //    side-by-side diff (Diff, ReviewQueuePage.tsx) of content/memorySeed/
  //    approvedContent/approvedMemorySeed, all four stripped from the
  //    default slim projection by Task 8. Without it the approver's diff
  //    panel is blank — approving without seeing what they approve, on the
  //    exact governance surface Phase 4 exists to provide. Bounded in
  //    practice: a review queue is small by definition, and the page limit
  //    still applies.
  return useAdminList<"artifacts", AdminArtifact>(
    ["admin", "reviewQueue"],
    "/v1/admin/artifacts?state=pending&include=content",
    "artifacts",
  );
}

const reviewKeys = [["admin", "artifacts"], ["admin", "reviewQueue"], ["catalog"]];

export const useSubmitArtifact = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/artifacts/${id}/submit`, t, { method: "POST" }),
    reviewKeys,
    "Submitted for review.",
  );

export const useWithdrawArtifact = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/artifacts/${id}/withdraw`, t, { method: "POST" }),
    reviewKeys,
    "Withdrawn from review.",
  );

export function useArtifactRevisions(id: string) {
  return useAdminList<"revisions", ArtifactRevision>(
    ["admin", "artifacts", id, "revisions"],
    `/v1/admin/artifacts/${id}/revisions`,
    "revisions",
    !!id,
  );
}

export const useRollbackArtifact = () =>
  useInvalidating(
    (t, a: { id: string; revision: number }) =>
      apiFetch(`/v1/admin/artifacts/${a.id}/rollback`, t, {
        method: "POST",
        body: { revision: a.revision },
      }),
    reviewKeys,
    "Rolled back.",
  );

/**
 * Sets (or, at 0, clears) the artifact's admin minimum-revision floor
 * (PUT /v1/admin/artifacts/{id}/min-revision, internal/api/admin_artifact_min_revision.ee.go).
 * rowVersion comes from the artifact object the caller already holds: in
 * ArtifactsPage that is `historyArtifact`, looked up fresh from the artifacts
 * list on every render (see its own comment: a prior mutation that
 * invalidates ["admin", "artifacts"] refreshes what the NEXT call here
 * carries). Quoted to match the server's strong ETag; an unquoted If-Match
 * is a 400.
 *
 * Not folded into reviewKeys: a floor changes no approval state and nothing
 * the review queue or catalog render, only ["admin", "artifacts"].
 */
export const useSetArtifactMinRevision = () =>
  useInvalidating(
    (t, a: { id: string; minRevision: number; rowVersion: number }) =>
      apiFetch(`/v1/admin/artifacts/${a.id}/min-revision`, t, {
        method: "PUT",
        body: { minRevision: a.minRevision },
        ifMatch: `"${a.rowVersion}"`,
      }),
    [["admin", "artifacts"]],
    "Minimum revision updated.",
  );

/**
 * The AUTHOR's own acknowledgment of the CURRENT scan findings on their own
 * pending submission (POST .../acknowledge-findings, docs/plans/orbeat-scan-
 * acknowledgment-2026-08-27.md). Submitter-only server-side (403 otherwise)
 * and refused with 412 when `digest` does not match the artifact's current
 * `scanFindingsDigest` -- a re-scan (withdraw/edit/resubmit) superseded it.
 * No If-Match: the digest itself IS this endpoint's precondition.
 */
export const useAcknowledgeFindings = () =>
  useInvalidating(
    (t, a: { id: string; digest: string }) =>
      apiFetch(`/v1/admin/artifacts/${a.id}/acknowledge-findings`, t, {
        method: "POST",
        body: { digest: a.digest },
      }),
    reviewKeys,
    "Findings acknowledged.",
  );

export const useApproveArtifact = () =>
  useInvalidating(
    // rowVersion comes from the review-queue row (useReviewQueue fetches
    // ?include=content, so the reviewer's diff and this precondition are
    // both drawn from the same read). Quoted to match the server's strong
    // ETag; an unquoted If-Match is a 400.
    //
    // acknowledgedFindingsDigest is the APPROVER's own acknowledgment of the
    // artifact's current findings digest (docs/plans/orbeat-scan-
    // acknowledgment-2026-08-27.md), sent ONLY when the caller supplies one
    // -- ReviewQueuePage sends it exactly when the artifact carries findings
    // AND the approver has ticked their own checkbox. Omitted entirely
    // otherwise (never an empty-string placeholder), which is what keeps a
    // clean artifact's approve request byte-identical to before this
    // feature: handleApproveArtifact's decodeOptionalJSON treats an absent
    // body as "no findings to acknowledge", not as a malformed one.
    (t, a: { id: string; rowVersion: number; acknowledgedFindingsDigest?: string }) =>
      apiFetch(`/v1/admin/artifacts/${a.id}/approve`, t, {
        method: "POST",
        ifMatch: `"${a.rowVersion}"`,
        ...(a.acknowledgedFindingsDigest !== undefined
          ? { body: { acknowledgedFindingsDigest: a.acknowledgedFindingsDigest } }
          : {}),
      }),
    reviewKeys,
    "Approved and distributed.",
  );

export const useRejectArtifact = () =>
  useInvalidating(
    (t, a: { id: string; reason: string }) =>
      apiFetch(`/v1/admin/artifacts/${a.id}/reject`, t, {
        method: "POST",
        body: { reason: a.reason },
      }),
    reviewKeys,
    "Rejected.",
  );

export function useMarketplaceStatus() {
  const { token } = useAuth();
  return useQuery({
    queryKey: ["admin", "publishStatus"],
    queryFn: ({ signal }) =>
      apiFetch<PublishStatus>("/v1/admin/marketplace/status", token, { signal }),
    refetchInterval: 2000,
  });
}

export function useMarketplacePublish() {
  const { token } = useAuth();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () =>
      apiFetch("/v1/admin/marketplace/publish", token, { method: "POST" }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["admin", "publishStatus"] }),
  });
}

export const useCreateRole = () =>
  useInvalidating(
    (t, name: string) =>
      apiFetch("/v1/admin/roles", t, { method: "POST", body: { name } }),
    [["admin", "roles"]],
    "Role created.",
  );

/**
 * Renames a role (PUT /v1/admin/roles/{id},
 * docs/plans/orbeat-role-rename-2026-08-27.md).
 *
 * rowVersion comes from the list row being edited, like useUpdateServer --
 * the roles list carries a real, non-zero version and there is no
 * GET /v1/admin/roles/{id}. Quoted to match the server's strong ETag; an
 * unquoted If-Match is a 400.
 *
 * idpRenamed is sent EXACTLY as the caller passes it -- never defaulted to
 * true here. RolesPage starts every edit session at `false` and only flips
 * it once the operator ticks the confirmation checkbox that appears after
 * the API asks for it (idpAssertionRequiredCode), so the first submit of any
 * rename always carries `idpRenamed: false`. Handing that decision to this
 * hook instead of the caller's own state would risk exactly the
 * pre-emptive assertion the design forbids: the API's `verifyIdpRename`
 * (internal/api/admin_roles.go) treats `idpRenamed` as authoritative only
 * when no realm-role lookup is configured, and a client that always sent
 * `true` would silently disable the one guard this feature exists to keep.
 */
export const useUpdateRole = () =>
  useInvalidating(
    (t, a: { id: string; name: string; idpRenamed: boolean; rowVersion: number }) =>
      apiFetch<Role>(`/v1/admin/roles/${a.id}`, t, {
        method: "PUT",
        body: { name: a.name, idpRenamed: a.idpRenamed },
        ifMatch: `"${a.rowVersion}"`,
      }),
    [["admin", "roles"]],
    "Role renamed.",
  );

export const useCreateEntitlement = () =>
  useInvalidating(
    (
      t,
      e: {
        roleId: string;
        mcpServerId: string;
        allowedTools: string[] | null;
      },
    ) =>
      apiFetch("/v1/admin/entitlements", t, { method: "POST", body: e }),
    [["admin", "entitlements"], ["catalog"]],
    "Entitlement granted.",
  );

export const useDeleteEntitlement = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/entitlements/${id}`, t, { method: "DELETE" }),
    [["admin", "entitlements"], ["catalog"]],
    "Entitlement revoked.",
  );

/**
 * order-only (ListOrderParams, not ListSearchParams): artifact-entitlements
 * REFUSE ?q= with 400 exactly like entitlements above, for the same reason
 * (see ListOrderParams's own comment): its params type carries no q field.
 */
export function useArtifactEntitlements(params: ListOrderParams = {}) {
  return useAdminList<"artifactEntitlements", ArtifactEntitlement>(
    ["admin", "artifactEntitlements"],
    "/v1/admin/artifact-entitlements",
    "artifactEntitlements",
    true,
    params,
  );
}

export const useCreateArtifactEntitlement = () =>
  useInvalidating(
    (t, e: { roleId: string; artifactId: string }) =>
      apiFetch("/v1/admin/artifact-entitlements", t, { method: "POST", body: e }),
    [["admin", "artifactEntitlements"]],
    "Artifact entitlement granted.",
  );

export const useDeleteArtifactEntitlement = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/artifact-entitlements/${id}`, t, { method: "DELETE" }),
    [["admin", "artifactEntitlements"]],
    "Artifact entitlement revoked.",
  );

/**
 * Virtual keys (Enterprise only, docs/specs/2026-08-25-orbeat-virtual-keys-
 * design.md sec 11): robot credentials owned by a role, narrowable to
 * specific tools, revocable instantly. Only ever reachable from
 * VirtualKeysPage, which itself renders nothing unless useVirtualKeysEnabled
 * (above) is true; see that hook's comment for why a Community caller must
 * never get far enough to invoke any of the three below.
 */
export function useVirtualKeys(params: ListSearchParams = {}) {
  return useAdminList<"virtualKeys", VirtualKey>(
    ["admin", "virtualKeys"],
    "/v1/admin/virtual-keys",
    "virtualKeys",
    true,
    params,
  );
}

export const useCreateVirtualKey = () =>
  useInvalidating(
    (t, input: VirtualKeyInput) =>
      apiFetch("/v1/admin/virtual-keys", t, { method: "POST", body: input }),
    [["admin", "virtualKeys"]],
    "Virtual key created.",
  );

/**
 * rowVersion comes from the list row being revoked (VirtualKeysPage has no
 * by-id fetch; the list row IS the only read this page ever does).
 * Quoted to match the server's strong ETag; an unquoted If-Match is a 400.
 */
export const useRevokeVirtualKey = () =>
  useInvalidating(
    (t, a: { id: string; rowVersion: number }) =>
      apiFetch(`/v1/admin/virtual-keys/${a.id}`, t, {
        method: "DELETE",
        ifMatch: `"${a.rowVersion}"`,
      }),
    [["admin", "virtualKeys"]],
    "Virtual key revoked.",
  );
