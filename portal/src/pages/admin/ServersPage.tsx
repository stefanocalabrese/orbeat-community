import { useState, type ChangeEvent } from "react";
import { useAdminServers, useCreateServer, useDeleteServer, useUpdateServer } from "../../api/queries";
import type { AdminServer, ServerInput, ServerUpdateInput } from "../../api/types";
import { errMsg } from "../../api/client";
import { FormField, inputCls } from "../../components/FormField";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { Pill } from "../../components/ui/Badge";
import { Card, Panel } from "../../components/ui/Card";
import { ListSearchBox, SortableTh } from "../../components/ui/AdminListControls";
import { ConflictNotice } from "./ConflictNotice";
import { isConflict } from "./conflict";
import { SEARCH_DEBOUNCE_MS, useDebouncedValue } from "../../hooks/useDebouncedValue";

const EMPTY: ServerInput = {
  name: "",
  description: "",
  transport: "http",
  endpointOrCommand: "",
  version: "",
  protocolVersion: "",
  secretRef: "",
  tlsCaRef: "",
  status: "active",
};

/**
 * Maps the edit form's plain-string field state onto the PUT wire body's
 * tri-state secretRef/tlsCaRef (defect 1, 2026-09-01): a non-empty field
 * value is always sent as the replacement; a blank field is sent as an
 * explicit "" ONLY when its "clear" checkbox was ticked, and otherwise left
 * `undefined` — which `JSON.stringify` (client.ts's apiFetchResponse) drops
 * from the request body entirely, so the API reads it as "not mentioned,
 * leave unchanged" rather than "clear".
 */
function toServerUpdateInput(v: ServerInput, clear: { secret: boolean; tlsCa: boolean }): ServerUpdateInput {
  return {
    name: v.name,
    description: v.description,
    transport: v.transport,
    endpointOrCommand: v.endpointOrCommand,
    version: v.version,
    protocolVersion: v.protocolVersion,
    status: v.status,
    secretRef: v.secretRef !== "" ? v.secretRef : clear.secret ? "" : undefined,
    tlsCaRef: v.tlsCaRef !== "" ? v.tlsCaRef : clear.tlsCa ? "" : undefined,
  };
}

interface ServerFormProps {
  initial: ServerInput;
  /**
   * clear reports which of the two "would clear" checkboxes below were
   * ticked at submit time (defect 1, 2026-09-01) — the create call site
   * ignores it entirely (create has no existing value to preserve, so there
   * is nothing to distinguish); the update call site uses it to decide
   * whether a blank field means "leave unchanged" (unticked) or "clear"
   * (ticked) when building the PUT body — see toServerUpdateInput below.
   */
  onSubmit: (v: ServerInput, clear: { secret: boolean; tlsCa: boolean }) => void;
  pending: boolean;
  error: string;
  /** Set only on the edit form — create can never 412 (no If-Match sent). */
  conflict?: boolean;
  onReload?: () => void;
  submitLabel: string;
  /**
   * The edit form always prefills Secret ref/TLS CA ref BLANK (the API never
   * echoes either — see AdminServer.hasSecret/hasTlsCa's own comment).
   * Before defect 1's fix (2026-09-01) the PUT was a naive full-replace, so
   * saving with the field still blank silently WIPED a credential the admin
   * never touched — the ORIGINAL form of the B12 guard blocked submission
   * until the admin either retyped the value or explicitly confirmed a
   * clear. The API now distinguishes "omitted" (leave unchanged) from
   * "explicit empty string" (clear), so leaving the field blank is SAFE BY
   * DEFAULT: submission is never blocked, and the checkbox below (still
   * present — "the guard stays") is now a POSITIVE affirmative action
   * ("check this to clear it") rather than a confirm-to-proceed gate.
   * Absent (create) or false means there is nothing configured yet to lose,
   * so the checkbox never renders.
   */
  hasSecret?: boolean;
  hasTlsCa?: boolean;
}

function ServerForm({
  initial,
  onSubmit,
  pending,
  error,
  conflict = false,
  onReload,
  submitLabel,
  hasSecret = false,
  hasTlsCa = false,
}: ServerFormProps) {
  const [v, setV] = useState(initial);
  // Ticked to actively CLEAR a configured ref (defect 1, 2026-09-01):
  // unticked + blank field means "leave unchanged" (the field is simply
  // omitted from the PUT body), ticked + blank field means "clear". Reset
  // doesn't need its own effect: the checkbox only RENDERS while the field
  // is blank (secretWouldClear/tlsCaWouldClear below), so typing a value
  // back in hides it, and its stale `true` is harmless because
  // toServerUpdateInput only consults it when the field is still blank.
  const [confirmClearSecret, setConfirmClearSecret] = useState(false);
  const [confirmClearTlsCa, setConfirmClearTlsCa] = useState(false);

  function set(k: keyof ServerInput) {
    return (e: ChangeEvent<HTMLInputElement | HTMLSelectElement>) => {
      setV({ ...v, [k]: e.target.value });
    };
  }

  // "Would clear" rather than "is blank": a brand-new create form's fields
  // start blank too, but hasSecret/hasTlsCa are false there (nothing exists
  // yet to lose), so the checkbox never renders on create.
  const secretWouldClear = hasSecret && v.secretRef === "";
  const tlsCaWouldClear = hasTlsCa && v.tlsCaRef === "";

  return (
    <Card className="mt-4 max-w-xl p-5">
      <form
        className="grid gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          onSubmit(v, { secret: confirmClearSecret, tlsCa: confirmClearTlsCa });
        }}
      >
        <FormField label="Name" htmlFor="f-name">
          <input id="f-name" className={inputCls} value={v.name} onChange={set("name")} required />
        </FormField>
        <FormField label="Description" htmlFor="f-desc">
          <input id="f-desc" className={inputCls} value={v.description} onChange={set("description")} />
        </FormField>
        <FormField label="Transport" htmlFor="f-tr">
          <select id="f-tr" className={inputCls} value={v.transport} onChange={set("transport")}>
            {/* stdio is intentionally absent: the remote gateway cannot dial a
                local stdio subprocess, so such a server would list in the catalog
                and yield zero tools. The API rejects it with a 400. */}
            <option value="http">http</option>
            <option value="sse">sse</option>
          </select>
        </FormField>
        <FormField label="Endpoint / command" htmlFor="f-ep">
          <input id="f-ep" className={inputCls} value={v.endpointOrCommand} onChange={set("endpointOrCommand")} required />
        </FormField>
        <FormField label="Version" htmlFor="f-version" hint="optional — upstream server version label (informational)">
          <input id="f-version" className={inputCls} value={v.version} onChange={set("version")} />
        </FormField>
        <FormField label="Protocol version" htmlFor="f-proto" hint="optional — MCP protocol version to pin/negotiate (e.g. 2025-06-18)">
          <input id="f-proto" className={inputCls} value={v.protocolVersion} onChange={set("protocolVersion")} />
        </FormField>
        <FormField
          label="Secret ref"
          htmlFor="f-secret"
          hint="e.g. env:ORBEAT_UPSTREAM_GITHUB_TOKEN (an env: name must match ORBEAT_SECRET_ENV_ALLOW, default ORBEAT_UPSTREAM_*); a reference, never the secret itself; empty = public upstream"
        >
          <input id="f-secret" className={inputCls} value={v.secretRef} onChange={set("secretRef")} />
        </FormField>
        {secretWouldClear && (
          <label className="-mt-1.5 flex items-start gap-2 text-xs text-warn">
            <input
              type="checkbox"
              checked={confirmClearSecret}
              onChange={(e) => setConfirmClearSecret(e.target.checked)}
              aria-label="This server has a secret configured. Check this box to clear it — otherwise leaving this field blank keeps the existing secret."
            />
            <span>This server has a secret configured. Leaving this blank keeps it — check to clear it instead.</span>
          </label>
        )}
        <FormField
          label="TLS CA ref"
          htmlFor="f-tlsca"
          hint="e.g. vault:pki/internal#ca_pem — a reference to a CA certificate (PEM), never the certificate itself; empty = verify against the system CA pool"
        >
          <input id="f-tlsca" className={inputCls} value={v.tlsCaRef} onChange={set("tlsCaRef")} />
        </FormField>
        {tlsCaWouldClear && (
          <label className="-mt-1.5 flex items-start gap-2 text-xs text-warn">
            <input
              type="checkbox"
              checked={confirmClearTlsCa}
              onChange={(e) => setConfirmClearTlsCa(e.target.checked)}
              aria-label="This server has a TLS CA configured. Check this box to clear it — otherwise leaving this field blank keeps the existing TLS CA."
            />
            <span>This server has a TLS CA configured. Leaving this blank keeps it — check to clear it instead.</span>
          </label>
        )}
        <FormField label="Status" htmlFor="f-st">
          <select id="f-st" className={inputCls} value={v.status} onChange={set("status")}>
            <option value="active">active</option>
            <option value="disabled">disabled</option>
          </select>
        </FormField>
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

export default function ServersPage() {
  // order/q both reset paging to page one on ANY change (queries.ts's
  // useAdminList folds them into the query key), which is the load-bearing
  // part of Task 5: the API binds a cursor to the sort/direction it was
  // minted under and 400s a replay under a different one.
  const [order, setOrder] = useState<"asc" | "desc">("asc");
  // `q` is the box's instant display value; the query itself is driven by
  // the debounced value below (B35: this used to fire one request per
  // keystroke — ListSearchBox's onChange fed the query key directly).
  const [q, setQ] = useState("");
  const debouncedQ = useDebouncedValue(q, SEARCH_DEBOUNCE_MS);
  const serversQuery = useAdminServers({ order, q: debouncedQ });
  const { rows: servers } = serversQuery;
  const create = useCreateServer();
  const update = useUpdateServer();
  const del = useDeleteServer();
  const [mode, setMode] = useState<"closed" | "create" | AdminServer>("closed");

  // Reset the mutation when (re)opening a form so a prior failed submit's error
  // never bleeds into a freshly opened form.
  const openCreate = () => {
    create.reset();
    setMode("create");
  };
  const openEdit = (s: AdminServer) => {
    update.reset();
    setMode(s);
  };

  const serversTable = (
    <Panel className="mt-4">
      <table className="w-full text-sm">
        <thead className="bg-inset">
          <tr>
            <SortableTh label="Name" order={order} onToggle={() => setOrder(order === "asc" ? "desc" : "asc")} />
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Transport</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Endpoint</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Status</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Auth</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {servers.map((s, i) => {
            const last = i === servers.length - 1;
            const cellCls = `p-3 ${last ? "" : "border-b border-border"}`;
            return (
              <tr key={s.id} className="hover:bg-inset">
                <td className={`${cellCls} font-medium text-text`}>{s.name}</td>
                <td className={`${cellCls} font-mono text-muted`}>{s.transport}</td>
                <td className={`max-w-56 truncate ${cellCls} text-muted`}>{s.endpointOrCommand}</td>
                <td className={cellCls}>
                  {s.status === "active" ? (
                    <Pill variant="ok" dot>{s.status}</Pill>
                  ) : (
                    <Pill variant="neutral">{s.status}</Pill>
                  )}
                </td>
                <td className={cellCls}>
                  <span className="inline-flex flex-wrap items-center gap-1">
                    {s.hasSecret ? (
                      <span className="inline-flex items-center rounded-full bg-warn-weak px-2.5 py-0.5 text-xs font-semibold text-warn">
                        secret
                      </span>
                    ) : (
                      <span className="text-xs text-faint">public</span>
                    )}
                    {/* hasTlsCa is why the API returns a flag rather than the
                        locator: the console must be able to show that an
                        upstream is pinned to a private CA without echoing the
                        reference. Without this the field is set, enforced by the
                        gateway, and invisible to the admin who set it. */}
                    {s.hasTlsCa && (
                      <span className="inline-flex items-center rounded-full bg-accent-weak px-2.5 py-0.5 text-xs font-semibold text-accent-ink">
                        TLS CA
                      </span>
                    )}
                  </span>
                </td>
                <td className={`${cellCls} text-right`}>
                  <button
                    onClick={() => openEdit(s)}
                    aria-label={`Edit ${s.name}`}
                    className="text-sm font-medium text-muted hover:text-text"
                  >
                    Edit
                  </button>
                  <button
                    onClick={() => {
                      if (window.confirm(`Delete ${s.name}?`)) del.mutate(s.id);
                    }}
                    aria-label={`Delete ${s.name}`}
                    className="ml-3 text-sm font-medium text-danger hover:text-danger"
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
          <p className="text-xs font-semibold uppercase tracking-wide text-faint">Gateway</p>
          <h1 className="mt-1 text-2xl font-semibold text-text">MCP servers</h1>
        </div>
        <div className="flex items-center gap-3">
          <ListSearchBox value={q} onChange={setQ} label="Search servers" />
          <Button variant="primary" onClick={openCreate}>
            New server
          </Button>
        </div>
      </div>

      {del.error && (
        <p className="mt-2 text-sm text-danger">Delete failed: {errMsg(del.error)}</p>
      )}

      {servers.length === 0 ? (
        // The gate owns loading/error for the first page; once any rows are
        // loaded, later pages are handled inline below so the accumulated
        // table never unmounts (see queries.ts's useAdminList doc comment).
        <QueryGate query={serversQuery} label="servers">
          {() => serversTable}
        </QueryGate>
      ) : (
        <>
          {serversTable}
          {serversQuery.isError && (
            // No separate Retry action: refetch() on an infinite query
            // re-fetches the last SUCCESSFUL page, not the failed next one.
            // "Load more" below (fetchNextPage) is the control that
            // correctly retries the page that just failed.
            <p className="mt-2 text-sm text-danger">
              Failed to load more servers: {errMsg(serversQuery.error)}
            </p>
          )}
          {serversQuery.isFetchingNextPage ? (
            <p className="mt-2 text-sm text-muted">Loading more…</p>
          ) : (
            serversQuery.hasNextPage && (
              <Button variant="ghost" className="mt-2" onClick={() => void serversQuery.fetchNextPage()}>
                Load more
              </Button>
            )
          )}
        </>
      )}

      {mode === "create" && (
        <ServerForm
          initial={EMPTY}
          submitLabel="Create"
          pending={create.isPending}
          error={errMsg(create.error)}
          onSubmit={(v) => create.mutate(v, { onSuccess: () => setMode("closed") })}
        />
      )}

      {mode !== "closed" && mode !== "create" && (
        <>
          <p className="mt-4 max-w-xl text-xs text-faint">
            Note: Secret ref and TLS CA ref are blank here even when the server has them set (the
            API returns whether they are configured, never the reference itself). Leaving either
            blank now PRESERVES it — nothing else about the form needs to change to keep a
            credential. To remove a configured reference instead, check its &quot;clear&quot; box
            below the field. Clearing TLS CA ref returns that upstream to the system CA pool.
          </p>
          <ServerForm
            key={mode.id}
            submitLabel="Save"
            pending={update.isPending}
            error={errMsg(update.error)}
            conflict={isConflict(update.error)}
            hasSecret={mode.hasSecret}
            hasTlsCa={mode.hasTlsCa}
            // Reload discards the in-progress edit (spec §10 / Task 11
            // judgment call): closing the form and refetching the list is
            // simpler and safer than trying to silently graft fresh field
            // values onto a form the admin was mid-typing in — the admin
            // reopens Edit to see the current row and reapply their change
            // from a clean, current baseline. See the Task 11 report for the
            // preserve-vs-discard tradeoff.
            onReload={() => {
              update.reset();
              setMode("closed");
              void serversQuery.refetch();
            }}
            initial={{
              name: mode.name,
              description: mode.description,
              transport: mode.transport,
              endpointOrCommand: mode.endpointOrCommand,
              version: mode.version,
              protocolVersion: mode.protocolVersion,
              secretRef: "",
              tlsCaRef: "",
              status: mode.status,
            }}
            onSubmit={(v, clear) =>
              update.mutate(
                { id: mode.id, input: toServerUpdateInput(v, clear), rowVersion: mode.rowVersion },
                { onSuccess: () => setMode("closed") },
              )
            }
          />
        </>
      )}
    </div>
  );
}
