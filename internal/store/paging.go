package store

import (
	"fmt"
	"strings"
)

// ListCursor is a keyset position: the sort-key values of the last row a page
// returned, plus id as the mandatory final tiebreaker.
//
// id is NOT optional and NOT per-table reasoning. Some paginated admin lists
// sort on a non-unique key (e.g. entitlement's role_id — a role has many
// entitlements), and keyset pagination on a non-unique key silently skips and
// duplicates rows across page boundaries. Appending id to every sort order and
// every cursor gives every list a total order by construction, so a list added
// later cannot re-arm that bug by picking a non-unique key.
type ListCursor struct {
	Keys []string
	ID   string
}

// sortKey is one column of a list's sort order.
//
// Col is the bare column name, not an expression: keysetTail emits it as
// `<table>.<col>`, so e.g. Col: "lower(name)" would render as
// `mcp_server.lower(name)`, which Postgres parses as a call to a function
// named "lower" in a schema named "mcp_server" and rejects. A list that ever
// needs to sort on an expression needs a different rendering than this one —
// Col does not support it.
//
// Cast is the Postgres type the cursor's placeholder for this column must be
// cast to. Cursor values travel as text, and the cast is what makes the
// placeholder COMPARABLE to the column at all — `revision_num < $1::text`
// raises `operator does not exist: integer < text` (42883) exactly as
// `id < $1::text` does for a uuid column. It is NOT what fixes the ORDER:
// ordering comes from `ORDER BY <table>.<col>`, always in the column's own
// type, so a wrong Cast fails loudly on the first page carrying a cursor
// rather than silently mis-ordering. (Verified on PG 16.14: integer<text,
// uuid<text, text<integer all 42883. The one pairing that WOULD coerce
// silently is timestamptz vs timestamp — no current key is a timestamp, and
// one added later must not assume the loud failure.)
//
// Col must reference a NOT NULL column. A row comparison (a, id) > ($1, $2)
// evaluates to NULL — never true — when a is NULL, so a row with a NULL sort
// key is silently dropped from every page after the first: the same class of
// silent row loss this package exists to prevent, just triggered by a
// different cause. Every current and planned sort-key column is NOT NULL;
// picking one that isn't needs its own design, not a key added here.
type sortKey struct {
	Col  string
	Cast string
}

// idKey is the tiebreaker appended to every list's sort order.
var idKey = sortKey{Col: "id", Cast: "uuid"}

// keysetTail renders the cursor predicate, ORDER BY and LIMIT tail of a keyset
// query, plus the args to append to the caller's own args (in that order). The
// caller's base query must already end in a WHERE clause — the predicate this
// function returns is prefixed " AND (...)", not "WHERE (...)".
//
// table is the table the sort keys belong to, and EVERY key is emitted
// table-qualified. That qualification is load-bearing, not cosmetic: this repo
// returns uuids as `id::text`, so the output column is LABELLED `id`, and
// Postgres resolves a bare name in ORDER BY against output labels before table
// columns. An unqualified `ORDER BY id` therefore sorts by the TEXT CAST, which
// no uuid index can serve — measured on entitlement's (role_id, id) keyset at
// 100k rows with a deep cursor: unqualified plans as a parallel Seq Scan +
// Sort (`Sort Key: ((role_id)::text), ((id)::text)`), qualified as a plain
// Index Scan using entitlement_tenant_role_id_idx. Orders of magnitude
// slower unqualified — independently measured at 61x, 187x and 349x across
// three separate measurements (original design-spec measurement, a spec
// review's reproduction, and this comment's own reproduction while
// investigating audit_event's much smaller win — see below); the exact
// ratio is environment- and run-dependent, as cost-model-driven timing
// comparisons generally are, but the PLAN-SHAPE difference (Seq Scan + Sort
// vs. Index Scan) is not, and that's the mechanism this qualification fixes.
// It generalizes to every list this function serves, but the SIZE of the win
// does not: it depends on how selective the leading key actually is at the
// table's real shape (see audit.go's ListAuditEventsPage / auditPageSQL for
// a case — audit_event — where table-qualifying still fixes a real defect
// but the measured win is much smaller and shape-dependent, because the
// deployment is single-tenant so the leading tenant_id key has ~no
// selectivity to exploit). WHERE is unaffected (output labels are not
// visible there), which is what makes the bug invisible: the predicate still
// uses the index-eligible column, only the sort falls off. Qualifying here
// means no caller can reintroduce it.
//
// keys are the sort columns WITHOUT the trailing id, which keysetTail appends.
// desc reverses both the ordering and the comparison direction, UNIFORMLY
// across every key — a Postgres row comparison cannot express mixed
// directions like (a ASC, b DESC), so a list needing that needs a different
// predicate shape entirely, not a per-key flag here. argN is how many
// placeholders the caller's WHERE clause already consumed, so numbering
// continues from there. limit <= 0 renders `LIMIT NULL` — Postgres' "no limit",
// as opposed to LIMIT 0 which returns nothing — which is how the unpaginated
// wrappers keep returning the full set through the same code path.
//
// The predicate is a Postgres row comparison ((a,b) > (x,y)) rather than a
// hand-written OR-chain: it is exactly keyset semantics, and the planner can
// drive a matching composite index with it directly.
//
// table, and every Col/Cast in keys, are interpolated directly into the SQL
// string and must be compile-time constants — never a value derived from a
// request. The only client-supplied input keysetTail handles is cursor's
// values, and those travel as query parameters ($N), never interpolated.
// keysetTail and sortKey are both unexported, and every call site in this
// package passes constants, so this holds by construction today; that is a
// property of the current call sites, not an enforced invariant — it does not
// need a typed-identifier or regex allowlist, which would be defending against
// an input this function can never actually receive.
func keysetTail(table string, keys []sortKey, desc bool, cursor *ListCursor, limit, argN int) (string, []any, error) {
	all := make([]sortKey, 0, len(keys)+1)
	all = append(all, keys...)
	all = append(all, idKey)

	qualified := make([]string, len(all))
	for i, k := range all {
		qualified[i] = table + "." + k.Col
	}

	var pred string
	var args []any

	if cursor != nil {
		if len(cursor.Keys) != len(keys) {
			return "", nil, fmt.Errorf("cursor has %d keys, want %d", len(cursor.Keys), len(keys))
		}
		vals := make([]string, 0, len(all))
		vals = append(vals, cursor.Keys...)
		vals = append(vals, cursor.ID)

		phs := make([]string, len(all))
		for i, k := range all {
			phs[i] = fmt.Sprintf("$%d::%s", argN+i+1, k.Cast)
			args = append(args, vals[i])
		}
		op := ">"
		if desc {
			op = "<"
		}
		pred = fmt.Sprintf(" AND (%s) %s (%s)",
			strings.Join(qualified, ", "), op, strings.Join(phs, ", "))
		argN += len(all)
	}

	dir := ""
	if desc {
		dir = " DESC"
	}
	ord := make([]string, len(all))
	for i, q := range qualified {
		ord[i] = q + dir
	}

	var limArg any = limit
	if limit <= 0 {
		limArg = nil
	}
	args = append(args, limArg)

	return fmt.Sprintf("%s ORDER BY %s LIMIT $%d",
		pred, strings.Join(ord, ", "), argN+1), args, nil
}
