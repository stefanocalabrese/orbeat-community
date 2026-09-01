import { useState } from "react";
import {
  useAdminArtifacts,
  useArtifactEntitlements,
  useCreateArtifactEntitlement,
  useDeleteArtifactEntitlement,
  useRoles,
} from "../../api/queries";
import { ApiRequestError, errMsg } from "../../api/client";
import { FormField, inputCls } from "../../components/FormField";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { Card, Panel } from "../../components/ui/Card";
import { SortableTh } from "../../components/ui/AdminListControls";

export default function ArtifactEntitlementsPage() {
  // order-only: artifact-entitlements REFUSE ?q= with 400 on mere presence
  // (useArtifactEntitlements's params type carries no q field at all), so
  // this page renders no search box; see ListSearchBox's own comment.
  const [order, setOrder] = useState<"asc" | "desc">("asc");
  const ents = useArtifactEntitlements({ order });
  const { rows: entitlements } = ents;
  const roles = useRoles();
  const artifacts = useAdminArtifacts();
  const create = useCreateArtifactEntitlement();
  const del = useDeleteArtifactEntitlement();
  const [open, setOpen] = useState(false);
  const [roleId, setRoleId] = useState("");
  const [artifactId, setArtifactId] = useState("");

  // roles/artifacts are cross-referenced by id here; only rows LOADED so far
  // (page 1, until that list's own "Load more" is clicked elsewhere) are
  // resolvable — an unresolved id falls back to displaying the raw id.
  const roleName = (id: string) =>
    roles.rows.find((r) => r.id === id)?.name ?? id;
  const artifactName = (id: string) =>
    artifacts.rows.find((a) => a.id === id)?.name ?? id;
  const roleArtifacts = artifacts.rows.filter((a) => a.visibility === "role");

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    create.mutate(
      {
        roleId: roleId || (roles.rows[0]?.id ?? ""),
        artifactId: artifactId || (roleArtifacts[0]?.id ?? ""),
      },
      {
        onSuccess: () => setOpen(false),
      },
    );
  }

  const entitlementsTable = (
    <Panel className="mt-4">
      <table className="w-full text-sm">
        <thead className="bg-inset">
          <tr>
            <SortableTh label="Role" order={order} onToggle={() => setOrder(order === "asc" ? "desc" : "asc")} />
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Artifact</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint" />
          </tr>
        </thead>
        <tbody>
          {entitlements.map((e, i) => {
            const last = i === entitlements.length - 1;
            const cellCls = `p-3 ${last ? "" : "border-b border-border"}`;
            return (
              <tr key={e.id} className="hover:bg-inset">
                <td className={`${cellCls} font-medium text-text`}>{roleName(e.roleId)}</td>
                <td className={`${cellCls} text-text`}>{artifactName(e.artifactId)}</td>
                <td className={`${cellCls} text-right`}>
                  <button
                    onClick={() => {
                      if (window.confirm("Delete entitlement?")) del.mutate(e.id);
                    }}
                    className="text-sm font-medium text-danger hover:text-danger"
                  >
                    Delete
                  </button>
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
      <div className="flex items-center justify-between">
        <div>
          <p className="text-xs font-semibold uppercase tracking-wide text-faint">Access control</p>
          <h1 className="mt-1 text-2xl font-semibold text-text">Artifact entitlements</h1>
        </div>
        <Button variant="primary" onClick={() => setOpen(true)}>
          New entitlement
        </Button>
      </div>
      <p className="mt-2 text-sm text-muted">
        Grant a role-visibility artifact to a role; it reaches entitled users via
        the sync client. Org-visibility artifacts ship to everyone via the native
        plugin and are not listed here.
      </p>

      {del.error && (
        <p className="mt-2 text-sm text-danger">Delete failed: {errMsg(del.error)}</p>
      )}

      {(roles.hasNextPage || artifacts.hasNextPage) && (
        <p className="mt-2 text-xs text-muted">
          {[roles.hasNextPage && "role", artifacts.hasNextPage && "artifact"].filter(Boolean).join(" and ")} names
          past the first page below show as a raw id — click Load more on the Roles/Artifacts page to resolve them.
        </p>
      )}

      {entitlements.length === 0 ? (
        // The gate owns loading/error for the first page; once any rows are
        // loaded, later pages are handled inline below so the accumulated
        // table never unmounts (see queries.ts's useAdminList doc comment).
        <QueryGate query={ents} label="artifact entitlements">
          {() => entitlementsTable}
        </QueryGate>
      ) : (
        <>
          {entitlementsTable}
          {ents.isError && (
            // No separate Retry action: refetch() on an infinite query
            // re-fetches the last SUCCESSFUL page, not the failed next one.
            // "Load more" below (fetchNextPage) is the control that
            // correctly retries the page that just failed.
            <p className="mt-2 text-sm text-danger">
              Failed to load more artifact entitlements: {errMsg(ents.error)}
            </p>
          )}
          {ents.isFetchingNextPage ? (
            <p className="mt-2 text-sm text-muted">Loading more…</p>
          ) : (
            ents.hasNextPage && (
              <Button variant="ghost" className="mt-2" onClick={() => void ents.fetchNextPage()}>
                Load more
              </Button>
            )
          )}
        </>
      )}

      {(roles.error || artifacts.error) && (
        <p className="mt-4 text-sm text-danger">
          Failed to load {roles.error ? "roles" : "artifacts"}: {errMsg(roles.error ?? artifacts.error)}
        </p>
      )}

      {open && (
        <Card className="mt-4 max-w-xl p-5">
          <form className="grid gap-3" onSubmit={handleSubmit}>
            <FormField
              label="Role"
              htmlFor="ae-role"
              hint={
                roles.hasNextPage
                  ? `Showing the first ${roles.limit ?? 100} roles by name — yours may not be listed yet`
                  : undefined
              }
            >
              <select
                id="ae-role"
                className={inputCls}
                value={roleId}
                onChange={(e) => setRoleId(e.target.value)}
              >
                {roles.rows.map((r) => (
                  <option key={r.id} value={r.id}>
                    {r.name}
                  </option>
                ))}
              </select>
            </FormField>
            {roleArtifacts.length === 0 ? (
              <p className="text-sm text-faint">
                {artifacts.hasNextPage ? (
                  // B35: `roleArtifacts` is filtered from only the artifacts
                  // LOADED so far (page 1 of useAdminArtifacts here) — zero
                  // matches on THIS page is not evidence zero exist overall,
                  // so the copy must not assert a global fact from a
                  // page-1 sample.
                  <>
                    None of the artifacts loaded so far are role-visibility, but more artifacts
                    exist that have not loaded yet. Click{" "}
                    <span className="font-medium text-text">Load more</span> on the Artifacts page,
                    or set an artifact's visibility to{" "}
                    <span className="font-medium text-text">role</span> there.
                  </>
                ) : (
                  <>
                    No role-visibility artifacts exist yet. Set an artifact's visibility to{" "}
                    <span className="font-medium text-text">role</span> on the Artifacts page
                    first.
                  </>
                )}
              </p>
            ) : (
              <FormField
                label="Artifact"
                htmlFor="ae-artifact"
                hint={
                  artifacts.hasNextPage
                    ? `Only role-visibility artifacts are entitle-able; showing the first ${artifacts.limit ?? 100} artifacts by name — yours may not be listed yet`
                    : "only role-visibility artifacts are entitle-able"
                }
              >
                <select
                  id="ae-artifact"
                  className={inputCls}
                  value={artifactId}
                  onChange={(e) => setArtifactId(e.target.value)}
                >
                  {roleArtifacts.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.name}
                    </option>
                  ))}
                </select>
              </FormField>
            )}
            {create.error instanceof ApiRequestError && (
              <p className="text-sm text-danger">{create.error.message}</p>
            )}
            <Button
              variant="primary"
              className="justify-self-start"
              disabled={create.isPending || roleArtifacts.length === 0}
            >
              Create
            </Button>
          </form>
        </Card>
      )}
    </div>
  );
}
