import { useState } from "react";
import { emptyAuditFilters, useAuditPage, type AuditFilters } from "../../api/queries";
import type { AuditEvent } from "../../api/types";
import { apiFetchRaw, errMsg, reportApiError } from "../../api/client";
import { QueryGate } from "../../components/QueryGate";
import { Button } from "../../components/ui/Button";
import { Panel } from "../../components/ui/Card";
import { JsonTree } from "../../components/ui/JsonTree";
import { useAuth } from "../../auth/useAuth";

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
  // B15: the export used to give no in-flight signal at all — the button
  // stayed clickable and nothing on screen said a request was even running,
  // so a hung export (previously with no timeout to end it) was
  // indistinguishable from a button that silently did nothing.
  const [exporting, setExporting] = useState(false);
  // `applied` is what the server is being asked for; `draft` is what is typed
  // into the form. Separating them is what keeps a request from firing on every
  // keystroke, and it is why the Apply button is the only thing that can change
  // which page is loaded.
  const [applied, setApplied] = useState<AuditFilters>(emptyAuditFilters);
  const [draft, setDraft] = useState<AuditFilters>(emptyAuditFilters);
  const auditQuery = useAuditPage(cursor, applied);
  const { data } = auditQuery;

  // The date range scopes ONLY the export (not the table below), and an inverted
  // range would silently produce an empty file — guard it.
  const rangeInvalid = from !== "" && to !== "" && from > to;

  async function exportAudit(format: "json" | "csv") {
    if (rangeInvalid || exporting) return;
    setExportMsg("");
    setExporting(true);
    const params = new URLSearchParams({ format });
    if (from) params.set("from", from);
    if (to) params.set("to", to);
    try {
      // apiFetchRaw (not a hand-rolled fetch): the same bearer-token
      // request, hard 30s timeout, and ApiRequestError body parse every
      // other admin request already gets. Returns the raw Response (rather
      // than apiFetch<T>'s parsed JSON) so the truncation header can still
      // be read before the body is consumed as a Blob.
      const res = await apiFetchRaw(`/v1/admin/audit/export?${params.toString()}`, token);
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
    } catch (e) {
      // The same 401 → re-login / 402 → cap-dialog policy every query and
      // mutation already gets (reportApiError's own comment on why this is
      // ONE function shared across every call site, not a copy per site).
      reportApiError(e);
      setExportMsg(`Export failed: ${errMsg(e)}`);
    } finally {
      setExporting(false);
    }
  }

  // Consume each fetched page exactly once, keyed on the (filters, cursor) pair
  // this page came from. Two mechanisms carry the whole behaviour and BOTH are
  // red-proven necessary: remove either one and the "applying a filter renders
  // the filtered page" test fails.
  //
  //  1. The key carries the FILTERS, not just the cursor. Applying a filter
  //     resets the cursor to "", so a cursor-only key would compare "" against
  //     the "" already consumed and skip the new first page, leaving the
  //     previous filter's rows on screen under the new filter's controls.
  //  2. An empty cursor REPLACES the rows; a non-empty one appends. An empty
  //     cursor can only be a first page, whether from the initial load or from
  //     the reset that applying performs, so replacing is what discards the
  //     rows the previous filter accumulated.
  //
  // Deliberately NOT here: an explicit setRows([]) inside applyFilters. It was,
  // and it made all three of these mutually redundant, so no test could fail
  // for any one of them. The visible difference is that the previous rows stay
  // up for the length of the fetch instead of blanking, which is the better of
  // the two anyway. Not a hook-rules violation: both branches are top-level.
  const pageKey = [applied.actor, applied.action, applied.decision, cursor].join("\u0000");
  const [seenKey, setSeenKey] = useState<string | null>(null);
  if (data && seenKey !== pageKey) {
    setRows((prev) =>
      cursor === "" ? data.events : [...prev, ...data.events.filter((e) => !prev.some((p) => p.id === e.id))],
    );
    setSeenKey(pageKey);
  }

  function applyFilters(next: AuditFilters) {
    setApplied(next);
    setDraft(next);
    setCursor("");
  }

  const filtering = applied.actor !== "" || applied.action !== "" || applied.decision !== "";

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
          Scopes the downloaded export only. It is independent of the table filters below: an
          export always contains every event in the range, whatever the table is showing.
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
          <Button variant="ghost" disabled={rangeInvalid || exporting} onClick={() => void exportAudit("json")}>
            {exporting ? "Exporting…" : "Export JSON"}
          </Button>
          <Button variant="ghost" disabled={rangeInvalid || exporting} onClick={() => void exportAudit("csv")}>
            {exporting ? "Exporting…" : "Export CSV"}
          </Button>
        </div>
        {rangeInvalid && (
          <p className="mt-2 text-sm text-danger">The From date must be on or before the To date.</p>
        )}
      </fieldset>
      {exportMsg && <p className="mt-2 text-sm text-warn">{exportMsg}</p>}

      <form
        className="mt-4 rounded-lg border border-border p-3"
        onSubmit={(e) => {
          e.preventDefault();
          applyFilters(draft);
        }}
      >
        <p className="px-1 text-xs font-semibold uppercase tracking-wide text-faint">Filter events</p>
        <p className="mb-2 mt-1 text-xs text-muted">
          Actor and action match exactly, not by prefix. Filters apply to the table only.
        </p>
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-xs font-medium text-muted">
            Actor
            <input
              type="text"
              aria-label="Filter by actor"
              value={draft.actor}
              onChange={(e) => setDraft({ ...draft, actor: e.target.value })}
              className="rounded-md border border-border-strong bg-surface px-2 py-1 text-sm text-text focus-visible:border-accent"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs font-medium text-muted">
            Action
            <input
              type="text"
              aria-label="Filter by action"
              value={draft.action}
              onChange={(e) => setDraft({ ...draft, action: e.target.value })}
              className="rounded-md border border-border-strong bg-surface px-2 py-1 text-sm text-text focus-visible:border-accent"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs font-medium text-muted">
            Decision
            <select
              aria-label="Filter by decision"
              value={draft.decision}
              onChange={(e) => setDraft({ ...draft, decision: e.target.value })}
              className="rounded-md border border-border-strong bg-surface px-2 py-1 text-sm text-text focus-visible:border-accent"
            >
              <option value="">Any</option>
              <option value="allow">allow</option>
              <option value="deny">deny</option>
              <option value="error">error</option>
            </select>
          </label>
          {/* Explicit types: Button renders a bare <button>, which inside a form
              defaults to type="submit" — an untyped Clear would submit the draft
              it is meant to discard. */}
          <Button type="submit">Apply filters</Button>
          <Button type="button" variant="ghost" disabled={!filtering && draft === applied} onClick={() => applyFilters(emptyAuditFilters)}>
            Clear
          </Button>
        </div>
      </form>

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
