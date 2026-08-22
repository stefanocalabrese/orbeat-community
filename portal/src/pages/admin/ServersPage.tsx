import { useState, type ChangeEvent } from "react";
import { useAdminServers, useCreateServer, useDeleteServer, useUpdateServer } from "../../api/queries";
import type { AdminServer, ServerInput } from "../../api/types";
import { errMsg } from "../../api/client";
import { FormField, inputCls } from "../../components/FormField";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { Pill } from "../../components/ui/Badge";
import { Card, Panel } from "../../components/ui/Card";
import { ConflictNotice } from "./ConflictNotice";
import { isConflict } from "./conflict";

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

interface ServerFormProps {
  initial: ServerInput;
  onSubmit: (v: ServerInput) => void;
  pending: boolean;
  error: string;
  /** Set only on the edit form — create can never 412 (no If-Match sent). */
  conflict?: boolean;
  onReload?: () => void;
  submitLabel: string;
}

function ServerForm({ initial, onSubmit, pending, error, conflict = false, onReload, submitLabel }: ServerFormProps) {
  const [v, setV] = useState(initial);

  function set(k: keyof ServerInput) {
    return (e: ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
      setV({ ...v, [k]: e.target.value });
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
          hint="e.g. env:GITHUB_TOKEN — a reference, never the secret itself; empty = public upstream"
        >
          <input id="f-secret" className={inputCls} value={v.secretRef} onChange={set("secretRef")} />
        </FormField>
        <FormField
          label="TLS CA ref"
          htmlFor="f-tlsca"
          hint="e.g. vault:pki/internal#ca_pem — a reference to a CA certificate (PEM), never the certificate itself; empty = verify against the system CA pool"
        >
          <input id="f-tlsca" className={inputCls} value={v.tlsCaRef} onChange={set("tlsCaRef")} />
        </FormField>
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
  const serversQuery = useAdminServers();
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
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Name</th>
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
        <Button variant="primary" onClick={openCreate}>
          New server
        </Button>
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
          <ServerForm
            key={mode.id}
            submitLabel="Save"
            pending={update.isPending}
            error={errMsg(update.error)}
            conflict={isConflict(update.error)}
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
            onSubmit={(v) =>
              update.mutate(
                { id: mode.id, input: v, rowVersion: mode.rowVersion },
                { onSuccess: () => setMode("closed") },
              )
            }
          />
          <p className="mt-2 max-w-xl text-xs text-faint">
            Note: PUT is full-replace. Both Secret ref and TLS CA ref are blank here even when the
            server has them set (the API returns whether they are configured, never the reference
            itself), so saving without re-entering a reference CLEARS it. Re-enter each one you want
            to keep. Clearing TLS CA ref returns that upstream to the system CA pool.
          </p>
        </>
      )}
    </div>
  );
}
