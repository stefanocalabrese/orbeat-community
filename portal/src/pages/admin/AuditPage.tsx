import { useState } from "react";
import { useAuditPage } from "../../api/queries";
import type { AuditEvent } from "../../api/types";
import { errMsg } from "../../api/client";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { Panel } from "../../components/ui/Card";
import { JsonTree } from "../../components/ui/JsonTree";
import { useAuth } from "../../auth/useAuth";
import { config } from "../../config";

const decisionCls: Record<string, string> = {
  allow: "text-ok bg-ok-weak",
  deny: "text-danger bg-danger-weak",
  error: "text-warn bg-warn-weak",
};

const METADATA_COLS = 6;

/**
 * fable-audit §7 #16 item 1: `metadata` arrives on every event (arbitrary
 * JSON, shape varies per `action` — see e.g. `role.delete`'s revoked-grants
 * payload, CHANGELOG v1.24.0) and was previously discarded client-side. Each
 * row owns its own expand state so opening one row's metadata never affects
 * another's, and rows with no metadata (an empty `{}`, common for simple
 * actions) get no control at all rather than an expander that opens onto
 * nothing.
 */
function AuditRow({ e, last }: { e: AuditEvent; last: boolean }) {
  const [open, setOpen] = useState(false);
  const hasMetadata = e.metadata != null && Object.keys(e.metadata).length > 0;
  const mainCellCls = `p-3 ${open || last ? "" : "border-b border-border"}`;
  const panelId = `audit-metadata-${e.id}`;

  return (
    <>
      <tr className="hover:bg-inset">
        <td className={`${mainCellCls} whitespace-nowrap font-mono text-muted`}>{new Date(e.ts).toLocaleString()}</td>
        <td className={`${mainCellCls} font-mono text-text`}>{e.actor}</td>
        <td className={`${mainCellCls} font-semibold text-accent-ink`}>{e.action}</td>
        <td className={`max-w-56 truncate ${mainCellCls} font-mono text-muted`}>{e.target}</td>
        <td className={mainCellCls}>
          <span
            className={`inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-semibold ${decisionCls[e.decision] ?? "text-muted bg-surface-2"}`}
          >
            {e.decision}
          </span>
        </td>
        <td className={mainCellCls}>
          {hasMetadata ? (
            <button
              type="button"
              aria-expanded={open}
              aria-controls={panelId}
              onClick={() => setOpen((o) => !o)}
              className="inline-flex items-center gap-1 rounded-md border border-border-strong px-2 py-1 text-xs font-medium text-muted hover:bg-surface-2 hover:text-text"
            >
              <span aria-hidden="true">{open ? "▾" : "▸"}</span>
              {open ? "Hide" : "Details"}
            </button>
          ) : (
            <span className="text-faint">—</span>
          )}
        </td>
      </tr>
      {open && hasMetadata && (
        <tr>
          <td id={panelId} colSpan={METADATA_COLS} className={`bg-inset p-3 ${last ? "" : "border-b border-border"}`}>
            <JsonTree value={e.metadata} />
          </td>
        </tr>
      )}
    </>
  );
}

export default function AuditPage() {
  const { token } = useAuth();
  const [cursor, setCursor] = useState("");
  const [rows, setRows] = useState<AuditEvent[]>([]);
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [exportMsg, setExportMsg] = useState("");
  const auditQuery = useAuditPage(cursor);
  const { data } = auditQuery;

  // The date range scopes ONLY the export (not the table below), and an inverted
  // range would silently produce an empty file — guard it.
  const rangeInvalid = from !== "" && to !== "" && from > to;

  async function exportAudit(format: "json" | "csv") {
    if (rangeInvalid) return;
    setExportMsg("");
    const params = new URLSearchParams({ format });
    if (from) params.set("from", from);
    if (to) params.set("to", to);
    try {
      const res = await fetch(`${config.apiBase}/v1/admin/audit/export?${params.toString()}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
      if (!res.ok) {
        setExportMsg(`Export failed (HTTP ${res.status}).`);
        return;
      }
      const truncated = res.headers.get("X-Orbeat-Export-Truncated") === "true";
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `orbeat-audit-export.${format}`;
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
      if (truncated) setExportMsg("Export reached the row cap and was truncated — narrow the date range for a complete export.");
    } catch {
      setExportMsg("Export failed — network error.");
    }
  }

  // Append newly fetched page exactly once per cursor value.
  // The seenCursor guard ensures this runs once per cursor — not a hook-rules
  // violation since both branches are top-level (not inside a hook).
  const [seenCursor, setSeenCursor] = useState<string | null>(null);
  if (data && seenCursor !== cursor) {
    setRows((prev) => [...prev, ...data.events.filter((e) => !prev.some((p) => p.id === e.id))]);
    setSeenCursor(cursor);
  }

  const auditTable = (
    <Panel className="mt-4">
      <table className="w-full text-sm">
        <thead className="bg-inset">
          <tr>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Time</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Actor</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Action</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Target</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Decision</th>
            <th className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint">Metadata</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((e, i) => (
            <AuditRow key={e.id} e={e} last={i === rows.length - 1} />
          ))}
        </tbody>
      </table>
    </Panel>
  );

  return (
    <div>
      <p className="text-xs font-semibold uppercase tracking-wide text-faint">Provenance</p>
      <h1 className="mt-1 text-2xl font-semibold text-text">Audit log</h1>

      <fieldset className="mt-4 rounded-lg border border-border p-3">
        <legend className="px-1 text-xs font-semibold uppercase tracking-wide text-faint">
          Export range
        </legend>
        <p className="mb-2 text-xs text-muted">
          Scopes the downloaded export only — the table below always shows the most recent events.
        </p>
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-xs font-medium text-muted">
            From
            <input
              type="date"
              aria-label="From date"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
              className="rounded-md border border-border-strong bg-surface px-2 py-1 text-sm text-text focus-visible:border-accent"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs font-medium text-muted">
            To
            <input
              type="date"
              aria-label="To date"
              value={to}
              onChange={(e) => setTo(e.target.value)}
              className="rounded-md border border-border-strong bg-surface px-2 py-1 text-sm text-text focus-visible:border-accent"
            />
          </label>
          <Button variant="ghost" disabled={rangeInvalid} onClick={() => exportAudit("json")}>Export JSON</Button>
          <Button variant="ghost" disabled={rangeInvalid} onClick={() => exportAudit("csv")}>Export CSV</Button>
        </div>
        {rangeInvalid && (
          <p className="mt-2 text-sm text-danger">The From date must be on or before the To date.</p>
        )}
      </fieldset>
      {exportMsg && <p className="mt-2 text-sm text-warn">{exportMsg}</p>}

      {rows.length === 0 ? (
        // First page: the gate owns loading/error; success renders the (possibly
        // genuinely empty) table.
        <QueryGate query={auditQuery} label="audit events">
          {() => auditTable}
        </QueryGate>
      ) : (
        // Accumulated rows stay mounted whatever the NEXT page's query is doing:
        // a pending page shows an inline indicator where Load more was, and a
        // failed page surfaces its error inline — never by unmounting the table.
        <>
          {auditTable}
          {auditQuery.isError && (
            <p className="mt-4 text-sm text-danger">
              Failed to load more audit events: {errMsg(auditQuery.error)}{" "}
              <button onClick={() => void auditQuery.refetch()} className="font-medium underline">
                Retry
              </button>
            </p>
          )}
          {auditQuery.isPending ? (
            <p className="mt-4 text-sm text-muted">Loading more…</p>
          ) : data?.nextCursor ? (
            <Button variant="ghost" className="mt-4" onClick={() => setCursor(data.nextCursor)}>
              Load more
            </Button>
          ) : null}
        </>
      )}
    </div>
  );
}
