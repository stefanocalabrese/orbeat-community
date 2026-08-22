import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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
  Page,
  PublishStatus,
  Role,
  RoleDeleteResult,
  ServerInput,
} from "./types";

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
 */
function useAdminList<K extends string, Row>(
  queryKey: readonly unknown[],
  path: string,
  rowsKey: K,
  enabled = true,
) {
  const { token } = useAuth();
  const query = useInfiniteQuery({
    queryKey,
    queryFn: ({ pageParam, signal }) =>
      apiFetch<Page<K, Row>>(
        // `path` may already carry its own query string (e.g. the review
        // queue's `?state=pending&include=content`), so the cursor param
        // must join with `&` in that case rather than a second `?`.
        pageParam
          ? `${path}${path.includes("?") ? "&" : "?"}cursor=${encodeURIComponent(pageParam)}`
          : path,
        token,
        { signal },
      ),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextCursor || undefined,
    enabled,
  });
  const rows = query.data?.pages.flatMap((p) => p[rowsKey]) ?? [];
  const limit = query.data?.pages[0]?.limit;
  return { ...query, rows, limit };
}

export function useCatalog() {
  const { token } = useAuth();
  return useQuery({
    queryKey: ["catalog"],
    queryFn: ({ signal }) =>
      apiFetch<{ servers: CatalogServer[] }>("/v1/catalog", token, { signal }),
  });
}

export function useAdminServers() {
  return useAdminList<"servers", AdminServer>(["admin", "servers"], "/v1/admin/servers", "servers");
}

export function useRoles() {
  return useAdminList<"roles", Role>(["admin", "roles"], "/v1/admin/roles", "roles");
}

export function useEntitlements() {
  return useAdminList<"entitlements", Entitlement>(
    ["admin", "entitlements"],
    "/v1/admin/entitlements",
    "entitlements",
  );
}

export function useAuditPage(cursor: string) {
  const { token } = useAuth();
  return useQuery({
    queryKey: ["admin", "audit", cursor],
    queryFn: ({ signal }) =>
      apiFetch<AuditPage>(
        `/v1/admin/audit?limit=50${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`,
        token,
        { signal },
      ),
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
) {
  const { token } = useAuth();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (a: TArgs) => fn(token, a),
    onSuccess: () =>
      keys.forEach((k) => void qc.invalidateQueries({ queryKey: k })),
  });
}

export const useCreateServer = () =>
  useInvalidating(
    (t, input: ServerInput) =>
      apiFetch("/v1/admin/servers", t, { method: "POST", body: input }),
    [["admin", "servers"], ["catalog"]],
  );

export const useUpdateServer = () =>
  useInvalidating(
    // rowVersion comes from the list row being edited (ServersPage prefills
    // its form from that row, and the list carries a real, non-zero version —
    // see AdminServer.rowVersion). Quoted to match the server's strong ETag;
    // an unquoted If-Match is a 400.
    (t, a: { id: string; input: ServerInput; rowVersion: number }) =>
      apiFetch(`/v1/admin/servers/${a.id}`, t, {
        method: "PUT",
        body: a.input,
        ifMatch: `"${a.rowVersion}"`,
      }),
    [["admin", "servers"], ["catalog"]],
  );

export const useDeleteServer = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/servers/${id}`, t, { method: "DELETE" }),
    [["admin", "servers"], ["catalog"]],
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
  );

export function useAdminArtifacts() {
  return useAdminList<"artifacts", AdminArtifact>(["admin", "artifacts"], "/v1/admin/artifacts", "artifacts");
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
  );

export const useDeleteArtifact = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/artifacts/${id}`, t, { method: "DELETE" }),
    [["admin", "artifacts"], ["catalog"]],
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
  );

export const useWithdrawArtifact = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/artifacts/${id}/withdraw`, t, { method: "POST" }),
    reviewKeys,
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
  );

export const useApproveArtifact = () =>
  useInvalidating(
    // rowVersion comes from the review-queue row (useReviewQueue fetches
    // ?include=content, so the reviewer's diff and this precondition are
    // both drawn from the same read). Quoted to match the server's strong
    // ETag; an unquoted If-Match is a 400.
    (t, a: { id: string; rowVersion: number }) =>
      apiFetch(`/v1/admin/artifacts/${a.id}/approve`, t, {
        method: "POST",
        ifMatch: `"${a.rowVersion}"`,
      }),
    reviewKeys,
  );

export const useRejectArtifact = () =>
  useInvalidating(
    (t, a: { id: string; reason: string }) =>
      apiFetch(`/v1/admin/artifacts/${a.id}/reject`, t, {
        method: "POST",
        body: { reason: a.reason },
      }),
    reviewKeys,
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
  );

export const useDeleteEntitlement = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/entitlements/${id}`, t, { method: "DELETE" }),
    [["admin", "entitlements"], ["catalog"]],
  );

export function useArtifactEntitlements() {
  return useAdminList<"artifactEntitlements", ArtifactEntitlement>(
    ["admin", "artifactEntitlements"],
    "/v1/admin/artifact-entitlements",
    "artifactEntitlements",
  );
}

export const useCreateArtifactEntitlement = () =>
  useInvalidating(
    (t, e: { roleId: string; artifactId: string }) =>
      apiFetch("/v1/admin/artifact-entitlements", t, { method: "POST", body: e }),
    [["admin", "artifactEntitlements"]],
  );

export const useDeleteArtifactEntitlement = () =>
  useInvalidating(
    (t, id: string) =>
      apiFetch(`/v1/admin/artifact-entitlements/${id}`, t, { method: "DELETE" }),
    [["admin", "artifactEntitlements"]],
  );
