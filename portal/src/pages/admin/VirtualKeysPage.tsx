import { useState } from "react";
import {
  useCreateVirtualKey,
  useMe,
  useRevokeVirtualKey,
  useRoles,
  useVirtualKeys,
} from "../../api/queries";
import { ApiRequestError, errMsg } from "../../api/client";
import { FormField, inputCls } from "../../components/FormField";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { Chip, Pill } from "../../components/ui/Badge";
import { Card, Panel } from "../../components/ui/Card";
import { ListSearchBox, SortableTh } from "../../components/ui/AdminListControls";
import { SEARCH_DEBOUNCE_MS, useDebouncedValue } from "../../hooks/useDebouncedValue";

/**
 * VirtualKeysPage: list, create, revoke robot credentials (docs/specs/
 * 2026-08-25-orbeat-virtual-keys-design.md sec 11).
 *
 * THE WHOLE PAGE IS GATED, not a control inside it (contrast ArtifactsPage's
 * pinning controls); see useVirtualKeysEnabled's own comment (api/queries.ts
 * — still used by AdminLayout's nav-item gate) for the general fail-closed
 * rationale, which still applies to the "still loading" and "Community"
 * cases below. The gate lives in this outer component rather than inside
 * VirtualKeysPageContent so the content component's own hooks
 * (useVirtualKeys, useRoles, the two mutations) never even mount when the
 * feature is off: nothing here ever issues GET /v1/admin/virtual-keys or GET
 * /v1/admin/roles against a server that may not serve either.
 *
 * B35: reads useMe() directly (rather than the collapsed useVirtualKeysEnabled
 * boolean) so a genuine GET /v1/me FAILURE can be told apart from "still
 * loading" and "Community" — all three used to render the exact same silent
 * `null`, an empty pane with no explanation. The nav link that points here
 * (AdminLayout) also hides on any of the three, but a stale bookmark or a
 * typed URL still reaches this component directly, so it has to explain a
 * real failure rather than look identical to "this feature doesn't exist".
 */
export default function VirtualKeysPage() {
  const me = useMe();
  if (me.isError) {
    return (
      <div className="mt-6 max-w-xl rounded-lg border border-danger bg-danger-weak p-4 text-sm text-danger">
        Could not determine whether virtual keys are available: {errMsg(me.error)}. Reload to try
        again.
      </div>
    );
  }
  if (me.data?.features?.virtualKeys !== true) return null;
  return <VirtualKeysPageContent />;
}

const EMPTY_JWKS = "";

function VirtualKeysPageContent() {
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
  const keysQuery = useVirtualKeys({ order, q: debouncedQ });
  const { rows: keys } = keysQuery;
  const roles = useRoles();
  const create = useCreateVirtualKey();
  const revoke = useRevokeVirtualKey();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [roleId, setRoleId] = useState("");
  const [jwksText, setJwksText] = useState(EMPTY_JWKS);
  const [jwksError, setJwksError] = useState<string | null>(null);
  const [tools, setTools] = useState("");

  const roleName = (id: string) => roles.rows.find((r) => r.id === id)?.name ?? id;

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    let jwks: unknown;
    try {
      // The robot's public JWKS is pasted as text and parsed here into a
      // real object BEFORE it reaches apiFetch's JSON.stringify: sending
      // the raw string through unparsed would double-encode it as a JSON
      // string value, which jwk.Parse on the server rejects (it wants key
      // material, not a string literal containing key material).
      jwks = JSON.parse(jwksText);
    } catch {
      setJwksError("jwks must be valid JSON. Paste the robot's public JWK or JWK Set.");
      return;
    }
    setJwksError(null);
    const allowedTools =
      tools.trim() === ""
        ? null
        : tools
            .split(",")
            .map((t) => t.trim())
            .filter(Boolean);
    create.mutate(
      {
        name,
        description,
        roleId: roleId || (roles.rows[0]?.id ?? ""),
        jwks,
        allowedTools,
      },
      {
        onSuccess: () => {
          setOpen(false);
          setName("");
          setDescription("");
          setJwksText(EMPTY_JWKS);
          setTools("");
        },
      },
    );
  }

  const keysTable = (
    <Panel className="mt-4">
      <table className="w-full text-sm">
        <thead className="bg-inset">
          <tr>
            <SortableTh label="Name" order={order} onToggle={() => setOrder(order === "asc" ? "desc" : "asc")} />
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Role</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Client ID</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Tools</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Status</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Created</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint" />
          </tr>
        </thead>
        <tbody>
          {keys.map((k, i) => {
            const last = i === keys.length - 1;
            const cellCls = `p-3 ${last ? "" : "border-b border-border"}`;
            return (
              <tr key={k.id} className="hover:bg-inset">
                <td className={`${cellCls} font-medium text-text`}>{k.name}</td>
                <td className={`${cellCls} text-text`}>{roleName(k.roleId)}</td>
                <td className={`${cellCls} font-mono text-xs text-muted`}>{k.clientId}</td>
                <td className={cellCls}>
                  {!k.allowedTools ? (
                    <span className="text-xs text-faint">all tools</span>
                  ) : (
                    <div className="flex flex-wrap gap-1">
                      {k.allowedTools.map((t) => (
                        <Chip key={t}>{t}</Chip>
                      ))}
                    </div>
                  )}
                </td>
                <td className={cellCls}>
                  {k.revoked ? <Pill variant="neutral">revoked</Pill> : <Pill variant="ok">active</Pill>}
                </td>
                <td className={`${cellCls} whitespace-nowrap font-mono text-xs text-muted`}>
                  {new Date(k.createdAt).toLocaleString()}
                </td>
                <td className={`${cellCls} text-right`}>
                  {!k.revoked && (
                    <Button
                      type="button"
                      variant="danger"
                      className="px-2.5 py-1 text-xs"
                      disabled={revoke.isPending && revoke.variables?.id === k.id}
                      aria-label={`Revoke ${k.name}`}
                      onClick={() => {
                        if (
                          window.confirm(
                            `Revoke virtual key "${k.name}"? Its robot is rejected on its very next call.`,
                          )
                        ) {
                          revoke.mutate({ id: k.id, rowVersion: k.rowVersion });
                        }
                      }}
                    >
                      Revoke
                    </Button>
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
          <h1 className="mt-1 text-2xl font-semibold text-text">Virtual keys</h1>
          <p className="mt-1 text-sm text-muted">
            Credentials for robots (CI jobs, scripts, unattended agents), owned by a role and narrowable to
            specific tools. No secret is ever generated, returned, or stored here.
          </p>
        </div>
        <div className="flex items-center gap-3">
          <ListSearchBox value={q} onChange={setQ} label="Search virtual keys" />
          <Button variant="primary" onClick={() => setOpen(true)}>
            New virtual key
          </Button>
        </div>
      </div>

      {revoke.error && (
        <p className="mt-2 text-sm text-danger">Revoke failed: {errMsg(revoke.error)}</p>
      )}

      {roles.hasNextPage && (
        <p className="mt-2 text-xs text-muted">
          Role names past the first page below show as a raw id; click Load more on the Roles page to
          resolve them.
        </p>
      )}

      {keys.length === 0 ? (
        // The gate owns loading/error for the first page; once any rows are
        // loaded, later pages are handled inline below so the accumulated
        // table never unmounts (see queries.ts's useAdminList doc comment).
        <QueryGate query={keysQuery} label="virtual keys">
          {() => keysTable}
        </QueryGate>
      ) : (
        <>
          {keysTable}
          {keysQuery.isError && (
            <p className="mt-2 text-sm text-danger">
              Failed to load more virtual keys: {errMsg(keysQuery.error)}
            </p>
          )}
          {keysQuery.isFetchingNextPage ? (
            <p className="mt-2 text-sm text-muted">Loading more…</p>
          ) : (
            keysQuery.hasNextPage && (
              <Button variant="ghost" className="mt-2" onClick={() => void keysQuery.fetchNextPage()}>
                Load more
              </Button>
            )
          )}
        </>
      )}

      {roles.error && (
        <p className="mt-4 text-sm text-danger">Failed to load roles: {errMsg(roles.error)}</p>
      )}

      {open && (
        <Card className="mt-4 max-w-xl p-5">
          <form className="grid gap-3" onSubmit={handleSubmit}>
            <FormField label="Name" htmlFor="vk-name">
              <input
                id="vk-name"
                className={inputCls}
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
              />
            </FormField>
            <FormField label="Description" htmlFor="vk-desc">
              <input
                id="vk-desc"
                className={inputCls}
                value={description}
                onChange={(e) => setDescription(e.target.value)}
              />
            </FormField>
            <FormField
              label="Role"
              htmlFor="vk-role"
              hint={
                roles.hasNextPage
                  ? `Showing the first ${roles.limit ?? 100} roles by name; yours may not be listed yet`
                  : "The key's effective access can never exceed this role's live entitlements."
              }
            >
              <select
                id="vk-role"
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
              label="Public JWKS"
              htmlFor="vk-jwks"
              hint="The robot's PUBLIC JSON Web Key or Key Set (RFC 7517). Never paste a private key; orbeat never asks for one."
            >
              <textarea
                id="vk-jwks"
                className={inputCls}
                rows={4}
                value={jwksText}
                onChange={(e) => setJwksText(e.target.value)}
                placeholder='{"keys":[{"kty":"RSA","kid":"robot-1","n":"...","e":"AQAB"}]}'
                required
              />
            </FormField>
            {jwksError && <p className="text-sm text-danger">{jwksError}</p>}
            <FormField
              label="Narrowed tools"
              htmlFor="vk-tools"
              hint="comma-separated; empty = everything the role allows. Can only ever remove access, never add to it."
            >
              <input
                id="vk-tools"
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
