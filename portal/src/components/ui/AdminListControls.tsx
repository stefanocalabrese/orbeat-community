import { inputCls } from "../FormField";

type Order = "asc" | "desc";

interface SortableThProps {
  label: string;
  order: Order;
  onToggle: () => void;
}

/**
 * A clickable admin-table column header that toggles ascending/descending
 * for the ONE column each admin list allows sorting on today
 * (docs/plans/orbeat-admin-search-sort-2026-08-27.md; internal/api/paging.go's
 * per-list allowlist admits exactly one column, already that list's existing
 * default order). Every other header in the same table stays a plain,
 * non-interactive <th>: a header that looks clickable and does nothing is
 * worse than one that plainly is not.
 *
 * type="button" is load-bearing on the inner button even though none of
 * these tables sit inside a <form>: a bare <button> defaults to
 * type="submit", and getting this wrong once (an accidental parent form
 * added later) would resubmit whatever form happened to wrap the table
 * instead of just toggling the sort.
 */
export function SortableTh({ label, order, onToggle }: SortableThProps) {
  return (
    <th
      className="border-b border-border p-3 text-left text-[11px] font-semibold uppercase tracking-wide text-faint"
      aria-sort={order === "asc" ? "ascending" : "descending"}
    >
      <button type="button" onClick={onToggle} className="inline-flex items-center gap-1 hover:text-text">
        {label}
        <span aria-hidden="true">{order === "asc" ? "▲" : "▼"}</span>
      </button>
    </th>
  );
}

interface ListSearchBoxProps {
  value: string;
  onChange: (v: string) => void;
  label: string;
}

/**
 * The ?q= search box for the four admin lists that support it
 * (docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 4). Entitlements
 * and artifact-entitlements never render this: the API refuses ?q= outright
 * on both, keyed on the parameter's mere PRESENCE (internal/api/paging.go's
 * refuseSearch uses Query().Has("q"), not a non-empty check), and their
 * hooks (useEntitlements/useArtifactEntitlements) accept no q field at all
 * -- there is nothing here for those two pages to wire up even by mistake.
 */
export function ListSearchBox({ value, onChange, label }: ListSearchBoxProps) {
  return (
    <input
      type="search"
      aria-label={label}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder="Search by name"
      className={`${inputCls} w-56`}
    />
  );
}
