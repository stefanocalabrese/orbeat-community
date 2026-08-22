import { useState } from "react";
import { useCreateRole, useDeleteRole, useRoles } from "../../api/queries";
import type { RoleDeleteResult } from "../../api/types";
import { ApiRequestError, errMsg } from "../../api/client";
import { inputCls } from "../../components/FormField";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { Panel } from "../../components/ui/Card";

export default function RolesPage() {
  const rolesQuery = useRoles();
  const { rows: roles } = rolesQuery;
  const create = useCreateRole();
  const del = useDeleteRole();
  const [name, setName] = useState("");
  // The confirm dialog is deliberately silent on counts (spec §6.1 — the
  // portal's own lists are capped, so a client-side count would understate
  // the blast radius on exactly the roles with the most grants). This is the
  // one place the admin sees what the cascade actually revoked, taken from
  // the DELETE response itself rather than computed here.
  const [deleted, setDeleted] = useState<{ name: string; result: RoleDeleteResult } | null>(null);

  const rolesList = (
    <Panel className="mt-4 max-w-md">
      <ul className="divide-y divide-border">
        {roles.map((r) => (
          <li key={r.id} className="flex items-center justify-between gap-3 p-3 text-sm font-medium text-text">
            <span>{r.name}</span>
            <Button
              type="button"
              variant="danger"
              className="px-2.5 py-1 text-xs"
              disabled={del.isPending && del.variables === r.id}
              aria-label={`Delete ${r.name}`}
              onClick={() => {
                if (
                  window.confirm(
                    `Delete role "${r.name}"? This also revokes every MCP server and artifact entitlement granted to it.`,
                  )
                ) {
                  setDeleted(null);
                  del.mutate(r.id, { onSuccess: (result) => setDeleted({ name: r.name, result }) });
                }
              }}
            >
              Delete
            </Button>
          </li>
        ))}
      </ul>
    </Panel>
  );

  return (
    <div>
      <p className="text-xs font-semibold uppercase tracking-wide text-faint">Access control</p>
      <h1 className="mt-1 text-2xl font-semibold text-text">Roles</h1>
      <p className="mt-1 text-sm text-muted">
        Role names must match the IdP realm roles (e.g. <code className="font-mono">orbeat-user</code>) — users
        are reconciled by name at login.
      </p>

      {del.error && (
        <p className="mt-2 max-w-md text-sm text-danger">Delete failed: {errMsg(del.error)}</p>
      )}
      {deleted && (
        <p className="mt-2 max-w-md text-sm text-muted">
          Deleted role &quot;{deleted.name}&quot; — revoked {deleted.result.entitlementsRevoked} MCP server
          entitlement{deleted.result.entitlementsRevoked === 1 ? "" : "s"} and{" "}
          {deleted.result.artifactEntitlementsRevoked} artifact entitlement
          {deleted.result.artifactEntitlementsRevoked === 1 ? "" : "s"}.
        </p>
      )}

      {roles.length === 0 ? (
        // The gate owns loading/error for the first page; once any rows are
        // loaded, later pages are handled inline below so the accumulated
        // list never unmounts (see queries.ts's useAdminList doc comment).
        <QueryGate query={rolesQuery} label="roles">
          {() => rolesList}
        </QueryGate>
      ) : (
        <>
          {rolesList}
          {rolesQuery.isError && (
            // No separate Retry action: refetch() on an infinite query
            // re-fetches the last SUCCESSFUL page, not the failed next one —
            // it would silently re-fetch page 1 again, not actually retry
            // this failure. "Load more" below (fetchNextPage) is the one
            // control that correctly retries the page that just failed.
            <p className="mt-2 max-w-md text-sm text-danger">
              Failed to load more roles: {errMsg(rolesQuery.error)}
            </p>
          )}
          {rolesQuery.isFetchingNextPage ? (
            <p className="mt-2 max-w-md text-sm text-muted">Loading more…</p>
          ) : (
            rolesQuery.hasNextPage && (
              <Button variant="ghost" className="mt-2" onClick={() => void rolesQuery.fetchNextPage()}>
                Load more
              </Button>
            )
          )}
        </>
      )}

      <form
        className="mt-4 flex max-w-md gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate(name, { onSuccess: () => setName("") });
        }}
      >
        <input
          aria-label="Role name"
          className={inputCls}
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="role name"
          required
        />
        <Button variant="primary" disabled={create.isPending}>
          Create
        </Button>
      </form>
      {create.error instanceof ApiRequestError && (
        <p className="mt-2 text-sm text-danger">{create.error.message}</p>
      )}
    </div>
  );
}
