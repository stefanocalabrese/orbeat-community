import { useState } from "react";
import { useCreateRole, useDeleteRole, useRoles, useUpdateRole } from "../../api/queries";
import type { Role, RoleDeleteResult } from "../../api/types";
import { ApiRequestError, errMsg } from "../../api/client";
import { inputCls } from "../../components/FormField";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { Panel } from "../../components/ui/Card";
import { ListSearchBox } from "../../components/ui/AdminListControls";
import { ConflictNotice } from "./ConflictNotice";
import { isConflict } from "./conflict";
import { isIdpAssertionRequired } from "./idpAssertion";
import { SEARCH_DEBOUNCE_MS, useDebouncedValue } from "../../hooks/useDebouncedValue";

export default function RolesPage() {
  // order/q both reset paging to page one on ANY change (queries.ts's
  // useAdminList folds them into the query key), which is the load-bearing
  // part of Task 5: the API binds a cursor to the sort/direction it was minted under
  // and 400s a replay under a different one.
  const [order, setOrder] = useState<"asc" | "desc">("asc");
  // `q` is the box's instant display value; the query itself is driven by
  // the debounced value below (B35: this used to fire one request per
  // keystroke — ListSearchBox's onChange fed the query key directly).
  const [q, setQ] = useState("");
  const debouncedQ = useDebouncedValue(q, SEARCH_DEBOUNCE_MS);
  const rolesQuery = useRoles({ order, q: debouncedQ });
  const { rows: roles } = rolesQuery;
  const create = useCreateRole();
  const del = useDeleteRole();
  const update = useUpdateRole();
  const [name, setName] = useState("");
  // The confirm dialog is deliberately silent on counts (spec §6.1 — the
  // portal's own lists are capped, so a client-side count would understate
  // the blast radius on exactly the roles with the most grants). What this
  // banner shows is taken from the DELETE response itself rather than
  // computed here.
  //
  // IT IS NOT WHAT THE CASCADE REVOKED, and this comment said it was until
  // A10 (2026-08-30). Deleting a role cascades to FIVE child tables: its
  // entitlements, its artifact entitlements, every virtual key bound to it,
  // its quota and its metering history. The 200 body carries two counts
  // (roleDeleteResponse, internal/api/admin_roles.go) and the type here
  // (RoleDeleteResult) has two fields to match, so this banner can only ever
  // name two of the five. The complete record is the role.delete audit
  // event, which A10 widened to all five and which is the only place the
  // client_id of each orphaned Keycloak client survives. Widening the body
  // to match is a separate change: portal/e2e/roles.spec.ts asserts it with
  // an exact toEqual, so it needs its own e2e run against the compose stack.
  // Until then the confirm copy below says which surface holds the rest,
  // rather than letting these two counts read as the whole story.
  const [deleted, setDeleted] = useState<{ name: string; result: RoleDeleteResult } | null>(null);

  // Rename: the row being edited (id, not index -- the list re-sorts and
  // re-pages under the user, exactly the reason EntitlementsPage's
  // editingId is keyed by id), its draft name, and the operator's tick on
  // the "I already renamed this in the identity provider" checkbox.
  // idpRenamed starts every edit session at false (startEdit below) and is
  // never defaulted to true: the checkbox itself is not even rendered until
  // the API's 400 asks for it (isIdpAssertionRequired(update.error)), so the
  // FIRST submit of any rename always carries idpRenamed: false -- the
  // portal must never decide this for the operator.
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [idpRenamed, setIdpRenamed] = useState(false);

  function startEdit(r: Role) {
    update.reset();
    setEditingId(r.id);
    setEditName(r.name);
    setIdpRenamed(false);
  }

  function cancelEdit() {
    update.reset();
    setEditingId(null);
  }

  function saveEdit(e: React.FormEvent, r: Role) {
    e.preventDefault();
    update.mutate(
      { id: r.id, name: editName, idpRenamed, rowVersion: r.rowVersion },
      { onSuccess: () => setEditingId(null) },
    );
  }

  const rolesList = (
    <Panel className="mt-4 max-w-md">
      <div className="flex items-center justify-between border-b border-border bg-inset p-3">
        <button
          type="button"
          onClick={() => setOrder(order === "asc" ? "desc" : "asc")}
          className="inline-flex items-center gap-1 text-[11px] font-semibold uppercase tracking-wide text-faint hover:text-text"
        >
          Name
          <span aria-hidden="true">{order === "asc" ? "▲" : "▼"}</span>
        </button>
      </div>
      <ul className="divide-y divide-border">
        {roles.map((r) =>
          editingId === r.id ? (
            <li key={r.id} className="p-3 text-sm text-text">
              <form onSubmit={(e) => saveEdit(e, r)}>
                <div className="flex items-center gap-2">
                  <input
                    aria-label={`New name for ${r.name}`}
                    className={inputCls}
                    value={editName}
                    onChange={(e) => setEditName(e.target.value)}
                    required
                  />
                  <Button disabled={update.isPending}>{update.isPending ? "Saving…" : "Save"}</Button>
                  {/* type="button" is load-bearing: without it this button
                      inside the form above defaults to type="submit" and
                      resubmits the very rename it is meant to discard --
                      the v1.23.0 Reload-button trap. */}
                  <Button type="button" variant="ghost" onClick={cancelEdit}>
                    Cancel
                  </Button>
                </div>

                {isConflict(update.error) ? (
                  <div className="mt-2">
                    <ConflictNotice
                      onReload={() => {
                        update.reset();
                        setEditingId(null);
                        void rolesQuery.refetch();
                      }}
                    />
                  </div>
                ) : isIdpAssertionRequired(update.error) ? (
                  // Shown ONLY on idpAssertionRequiredCode -- a 409 name
                  // collision or the "no such realm role" 400 a configured
                  // lookup returns are not the operator's to override (see
                  // isIdpAssertionRequired's doc comment) and fall through
                  // to the plain message branch below instead.
                  <div className="mt-2 rounded-lg border border-warn bg-warn-weak px-3 py-2 text-sm text-warn">
                    <p>{update.error.message}</p>
                    <label className="mt-2 flex items-start gap-2">
                      <input
                        type="checkbox"
                        checked={idpRenamed}
                        onChange={(e) => setIdpRenamed(e.target.checked)}
                      />
                      <span>
                        orbeat matches roles to the identity provider by name. If this role has not
                        already been renamed in Keycloak, every user holding it loses it immediately.
                        Its entitlements survive, so nothing is deleted, but nobody holds the role
                        any more. I confirm the role was already renamed in the identity provider.
                      </span>
                    </label>
                  </div>
                ) : (
                  update.error && <p className="mt-2 text-sm text-danger">{errMsg(update.error)}</p>
                )}
              </form>
            </li>
          ) : (
            <li key={r.id} className="flex items-center justify-between gap-3 p-3 text-sm font-medium text-text">
              <span>{r.name}</span>
              <div className="flex items-center gap-3">
                <button
                  type="button"
                  onClick={() => startEdit(r)}
                  aria-label={`Rename ${r.name}`}
                  className="text-sm font-medium text-accent-ink hover:underline"
                >
                  Rename
                </button>
                <Button
                  type="button"
                  variant="danger"
                  className="px-2.5 py-1 text-xs"
                  disabled={del.isPending && del.variables === r.id}
                  aria-label={`Delete ${r.name}`}
                  onClick={() => {
                    if (
                      window.confirm(
                        `Delete role "${r.name}"? The cascade destroys more than entitlements: ` +
                          `every MCP server and artifact entitlement granted to it, every virtual key bound ` +
                          `to it, its quota and its usage history. This page reports only the two ` +
                          `entitlement counts; the role.delete audit event lists all five, including the ` +
                          `client_id of each Keycloak client this orphans.`,
                      )
                    ) {
                      setDeleted(null);
                      del.mutate(r.id, { onSuccess: (result) => setDeleted({ name: r.name, result }) });
                    }
                  }}
                >
                  Delete
                </Button>
              </div>
            </li>
          ),
        )}
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

      <div className="mt-3">
        <ListSearchBox value={q} onChange={setQ} label="Search roles" />
      </div>

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
