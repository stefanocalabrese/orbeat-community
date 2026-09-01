import { useState } from "react";
import {
  useAdminServers,
  useCreateEntitlement,
  useDeleteEntitlement,
  useEntitlements,
  useRoles,
  useUpdateEntitlement,
} from "../../api/queries";
import { ApiRequestError, errMsg } from "../../api/client";
import { FormField, inputCls } from "../../components/FormField";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { Chip } from "../../components/ui/Badge";
import { Card, Panel } from "../../components/ui/Card";
import { SortableTh } from "../../components/ui/AdminListControls";

export default function EntitlementsPage() {
  // order-only: entitlements REFUSE ?q= with 400 on mere presence
  // (useEntitlements's params type carries no q field at all), so this page
  // renders no search box; see ListSearchBox's own comment.
  const [order, setOrder] = useState<"asc" | "desc">("asc");
  const ents = useEntitlements({ order });
  const { rows: entitlements } = ents;
  const roles = useRoles();
  const servers = useAdminServers();
  const create = useCreateEntitlement();
  const del = useDeleteEntitlement();
  const update = useUpdateEntitlement();
  const [open, setOpen] = useState(false);
  // The row being edited, and the draft tool list for it. Keyed by id rather
  // than by index: the list re-sorts and re-pages under the user, and an index
  // would silently start editing a different grant.
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editTools, setEditTools] = useState("");
  const [roleId, setRoleId] = useState("");
  const [serverId, setServerId] = useState("");
  const [tools, setTools] = useState("");

  // roles/servers are cross-referenced by id here; only rows LOADED so far
  // (page 1, until that list's own "Load more" is clicked elsewhere) are
  // resolvable — an unresolved id falls back to displaying the raw id.
  const roleName = (id: string) =>
    roles.rows.find((r) => r.id === id)?.name ?? id;
  const serverName = (id: string) =>
    servers.rows.find((s) => s.id === id)?.name ?? id;

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    const allowedTools =
      tools.trim() === ""
        ? null
        : tools
            .split(",")
            .map((t) => t.trim())
            .filter(Boolean);
    create.mutate(
      {
        roleId: roleId || (roles.rows[0]?.id ?? ""),
        mcpServerId: serverId || (servers.rows[0]?.id ?? ""),
        allowedTools,
      },
      {
        onSuccess: () => {
          setOpen(false);
          setTools("");
        },
      },
    );
  }

  // "" means every tool, matching the API's `allowedTools: null`. The empty
  // string cannot mean "no tools": a grant that allows nothing is a grant that
  // should be deleted, and letting a blank field mean that would turn a
  // half-finished edit into a silent revocation.
  const parseTools = (raw: string): string[] | null => {
    const parts = raw.split(",").map((t) => t.trim()).filter(Boolean);
    return parts.length === 0 ? null : parts;
  };

  function startEdit(id: string, allowedTools: string[] | null) {
    // Reset the mutation when (re)opening an edit, same as every other
    // admin form's openCreate/openEdit — otherwise a stale 412 from a
    // PRIOR attempt (on this row or another) outlives the edit that caused
    // it, including surviving Cancel below, which discards the draft but
    // not the mutation's own error state.
    update.reset();
    setEditingId(id);
    setEditTools(allowedTools === null ? "" : allowedTools.join(", "));
  }

  function cancelEdit() {
    update.reset();
    setEditingId(null);
  }

  function saveEdit(e: React.FormEvent, row: { id: string; permissions: string[]; rowVersion: number }) {
    e.preventDefault();
    update.mutate(
      {
        id: row.id,
        allowedTools: parseTools(editTools),
        // Permissions are not editable here and are echoed back unchanged: the
        // PUT is a full replace, so omitting them would silently clear them.
        permissions: row.permissions,
        rowVersion: row.rowVersion,
      },
      { onSuccess: () => setEditingId(null) },
    );
  }

  const entitlementsTable = (
    <Panel className="mt-4">
      <table className="w-full text-sm">
        <thead className="bg-inset">
          <tr>
            <SortableTh label="Role" order={order} onToggle={() => setOrder(order === "asc" ? "desc" : "asc")} />
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Server</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Tools</th>
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
                <td className={`${cellCls} text-text`}>{serverName(e.mcpServerId)}</td>
                <td className={cellCls}>
                  {editingId === e.id ? (
                    <form id={`edit-${e.id}`} onSubmit={(ev) => saveEdit(ev, e)}>
                      <input
                        type="text"
                        aria-label={`Allowed tools for ${roleName(e.roleId)} on ${serverName(e.mcpServerId)}`}
                        value={editTools}
                        onChange={(ev) => setEditTools(ev.target.value)}
                        placeholder="leave empty for all tools"
                        className={inputCls}
                      />
                    </form>
                  ) : e.allowedTools === null ? (
                    <span className="text-xs text-faint">all tools</span>
                  ) : (
                    <div className="flex flex-wrap gap-1">
                      {e.allowedTools.map((t) => (
                        <Chip key={t}>{t}</Chip>
                      ))}
                    </div>
                  )}
                </td>
                <td className={`${cellCls} text-right`}>
                  {editingId === e.id ? (
                    <div className="flex justify-end gap-2">
                      {/* type is explicit on both: a bare <button> inside a
                          form defaults to submit, so an untyped Cancel would
                          save the edit it is meant to discard. */}
                      <Button type="submit" form={`edit-${e.id}`} disabled={update.isPending}>
                        {update.isPending ? "Saving…" : "Save"}
                      </Button>
                      <Button type="button" variant="ghost" onClick={cancelEdit}>
                        Cancel
                      </Button>
                    </div>
                  ) : (
                    <div className="flex justify-end gap-3">
                      <button
                        type="button"
                        onClick={() => startEdit(e.id, e.allowedTools)}
                        className="text-sm font-medium text-accent-ink hover:underline"
                      >
                        Edit tools
                      </button>
                      <button
                        onClick={() => {
                          if (window.confirm("Delete entitlement?")) del.mutate(e.id);
                        }}
                        className="text-sm font-medium text-danger hover:text-danger"
                      >
                        Delete
                      </button>
                    </div>
                  )}
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
          <h1 className="mt-1 text-2xl font-semibold text-text">Entitlements</h1>
        </div>
        <Button variant="primary" onClick={() => setOpen(true)}>
          New entitlement
        </Button>
      </div>

      {update.error && (
        <p className="mt-4 text-sm text-danger">
          {update.error instanceof ApiRequestError && update.error.status === 412
            ? "Someone else changed this entitlement while you were editing. Reload the page and reapply your change."
            : `Failed to update entitlement: ${errMsg(update.error)}`}
        </p>
      )}

      {del.error && (
        <p className="mt-2 text-sm text-danger">Delete failed: {errMsg(del.error)}</p>
      )}

      {(roles.hasNextPage || servers.hasNextPage) && (
        <p className="mt-2 text-xs text-muted">
          {[roles.hasNextPage && "role", servers.hasNextPage && "server"].filter(Boolean).join(" and ")} names
          past the first page below show as a raw id — click Load more on the Roles/Servers page to resolve them.
        </p>
      )}

      {entitlements.length === 0 ? (
        // The gate owns loading/error for the first page; once any rows are
        // loaded, later pages are handled inline below so the accumulated
        // table never unmounts (see queries.ts's useAdminList doc comment).
        <QueryGate query={ents} label="entitlements">
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
              Failed to load more entitlements: {errMsg(ents.error)}
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

      {(roles.error || servers.error) && (
        <p className="mt-4 text-sm text-danger">
          Failed to load {roles.error ? "roles" : "servers"}: {errMsg(roles.error ?? servers.error)}
        </p>
      )}

      {open && (
        <Card className="mt-4 max-w-xl p-5">
          <form className="grid gap-3" onSubmit={handleSubmit}>
            <FormField
              label="Role"
              htmlFor="e-role"
              hint={
                roles.hasNextPage
                  ? `Showing the first ${roles.limit ?? 100} roles by name — yours may not be listed yet`
                  : undefined
              }
            >
              <select
                id="e-role"
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
            <FormField
              label="Server"
              htmlFor="e-srv"
              hint={
                servers.hasNextPage
                  ? `Showing the first ${servers.limit ?? 100} servers by name — yours may not be listed yet`
                  : undefined
              }
            >
              <select
                id="e-srv"
                className={inputCls}
                value={serverId}
                onChange={(e) => setServerId(e.target.value)}
              >
                {servers.rows.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
              </select>
            </FormField>
            <FormField
              label="Allowed tools"
              htmlFor="e-tools"
              hint="comma-separated; empty = all tools"
            >
              <input
                id="e-tools"
                className={inputCls}
                value={tools}
                onChange={(e) => setTools(e.target.value)}
                placeholder="echo, create_issue"
              />
            </FormField>
            {create.error instanceof ApiRequestError && (
              <p className="text-sm text-danger">{create.error.message}</p>
            )}
            <Button variant="primary" className="justify-self-start" disabled={create.isPending}>
              Create
            </Button>
          </form>
        </Card>
      )}
    </div>
  );
}
