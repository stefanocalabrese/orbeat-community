package store

import (
	"errors"
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
//
// Sort binds the cursor to the sort order it was minted under. Without it, a
// cursor minted while sorting by one column and replayed while sorting by a
// DIFFERENT column of the same key count is indistinguishable, at the shape
// level, from a legitimate cursor: same number of keys, same Cast, same
// tiebreaker. keysetTail's key-count check (below) catches a cursor from a
// two-key list replayed against a one-key list; it does not, and cannot,
// catch two single-key sorts of the same list, that mismatch has the right
// shape and the wrong meaning, and the walk silently skips or repeats rows
// with no error anywhere (reproduced in paging_test.go's
// TestCursorSortMismatchIsRefused). Sort closes exactly that gap.
//
// Sort is set by sortIdentity(table, keys), the SAME keys slice keysetTail
// renders SQL from, both when a cursor is minted (every XCursor constructor)
// and when one is checked (keysetTail). It is never a separate literal a
// constructor could type wrong or forget to update: the identity is whatever
// the query actually sorts by, mechanically, so it cannot drift out of sync
// with the SQL the way a hand-maintained enum value could.
//
// Sort is NOT a security boundary and must never be treated as one: a client
// controls the whole opaque cursor already (Keys and ID included), so nothing
// stops it from setting Sort to whatever string matches the sort it wants.
// The point is not to stop an adversary, an adversary gains nothing by
// forging it, since a valid cursor for a given sort already walks that sort
// correctly, it is to stop an HONEST client (or a genuine bug) from
// replaying yesterday's cursor under today's different sort and getting
// silently wrong rows back instead of a loud, obvious error.
type ListCursor struct {
	Keys []string
	ID   string
	Sort string
}

// ErrCursorSortMismatch is returned by keysetTail when cursor.Sort does not
// match the sort actually being walked, including the empty string, which
// is what a cursor minted before this field existed decodes to. That is a
// deliberate choice, not an oversight: an old cursor has no way to state
// which sort it was minted under, and the alternative, treating a blank
// Sort as "matches anything", would silently accept it against whatever
// sort the caller is currently walking, which is precisely the silent
// mis-read this type exists to prevent. A cursor that predates this field
// gets refused and the client starts over at page one, exactly like any
// other mismatch.
var ErrCursorSortMismatch = errors.New("cursor was minted under a different sort")

// sortIdentity derives a cheap, mechanically-computed identifier for a sort
// order from the table, its ordered column names (the trailing id tiebreaker
// is excluded: every list carries it, so it never distinguishes anything),
// and its direction. Calling this with the same (table, keys, desc) a query
// actually uses -- rather than hand-writing a separate label, is what keeps a
// cursor's recorded sort from ever drifting out of sync with the SQL it was
// minted against: the identity changes exactly when, and only when, the
// rendered ORDER BY does.
//
// desc is part of the identity for the SAME reason column identity is
// (paging.go's ListCursor doc comment has the full case for columns): once
// ?order is client-controlled (docs/plans/orbeat-admin-search-sort-2026-08-27.md
// Task 3), a cursor minted while walking ascending and replayed while walking
// descending has the right key COUNT and the right Cast, but the comparison
// operator keysetTail emits flips (">" vs "<") between the two -- the same
// "right shape, wrong meaning" mismatch TestCursorSortMismatchIsRefused
// reproduces for columns, just triggered by direction instead. Folding desc
// into this one identity string, rather than adding a second field to
// ListCursor, reuses the exact mismatch check keysetTail already runs: no new
// comparison, no new error path, nothing that could check one axis and forget
// the other.
func sortIdentity(table string, keys []sortKey, desc bool) string {
	cols := make([]string, len(keys))
	for i, k := range keys {
		cols[i] = k.Col
	}
	dir := "asc"
	if desc {
		dir = "desc"
	}
	return table + ":" + strings.Join(cols, ",") + ":" + dir
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
//
// A non-nil cursor is checked TWICE before it is trusted, and the two checks
// guard different things, deliberately kept separate rather than folded into
// one:
//   - key COUNT (len(cursor.Keys) == len(keys)) guards structural shape --
//     without it, a shorter or longer Keys slice misaligns the positional
//     read below (vals[i] for i over range(all)), either silently dropping
//     the real id in favor of a stray key value, or indexing out of range.
//   - sort IDENTITY (cursor.Sort == sortIdentity(table, keys)) guards
//     semantic meaning, a cursor can have exactly the right key COUNT and
//     still have been minted under a different sort of that same count
//     (ListCursor's own doc comment has the full case). Neither check can
//     stand in for the other: a forged or corrupted cursor can present any
//     combination of a wrong-length Keys with a right-looking Sort, or vice
//     versa, since they are independent fields on the same struct.
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
		if want := sortIdentity(table, keys, desc); cursor.Sort != want {
			return "", nil, fmt.Errorf("%w: cursor sort %q, this list sorts by %q", ErrCursorSortMismatch, cursor.Sort, want)
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

// likeSearchArg turns an admin ?q= substring search term
// (docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 4) into the exact
// value a page query should bind for a "$N::text IS NULL OR <col> ILIKE $N"
// filter: the SAME two-state-NULL shape artifactPageSQL's ?state and
// virtualKeyPageSQL's ?revoked already use (both in this same package), so an
// absent search renders identically to theirs: a bound NULL that the OR
// short-circuits past, not an empty-but-present pattern that a naive
// "%%" would also happen to match everything for. An empty term returns nil
// for exactly that reason: a cleared search box should show every row, not
// zero, and NULL is what makes that the same code path as "no filter"
// rather than a separate case to get right twice.
//
// ILIKE, not LIKE: case-insensitive, deliberately. An admin typing a server
// or role name from memory has no reason to know or reproduce its exact
// casing (mcp_server.name is free text, and validEndpoint/checkServerSlugCollision
// admit "My Server" as a real example; role/virtual_key names carry no
// case constraint either), so a case-sensitive box would silently miss exact
// matches that differ only in case, which reads as "search is broken" rather
// than "search is precise". This is independent of, and does not reopen,
// paging.go's OWN "case-insensitive SORT is out" decision above (sortKey.Col
// doc comment): that decision is about rendering an ORDER BY as
// `lower(<table>.<col>)`, which needs a different Col shape and a matching
// expression index; nothing here changes the ORDER BY, and ILIKE against a
// plain column is an ordinary WHERE clause with no such rendering conflict.
//
// The scan this filter costs is unconditional, not a consequence of case
// sensitivity: a substring match needs a LEADING '%', and a leading wildcard
// defeats a btree index (b-trees are usable only for prefix/range
// comparisons) whether the match is ILIKE or plain LIKE: there is no
// case-sensitive variant of this filter that would have been index-servable
// instead. Task 2's "sort keys must be indexed, or the sort is refused" rule
// therefore does not apply here: that rule polices the ORDER BY, which every
// list still walks via its existing keyset index exactly as before, and a
// sequential-scan FILTER stacked on top of an index-driven keyset walk is a
// different (and here, accepted) cost, not the v1.22.0 defect class that
// rule closes. Accepted at today's scale: Community caps a tenant at 10
// servers / 10 active servers / 1 role (editionlimits.go), and even an
// unbounded Enterprise tenant's admin console tables are still small enough
// (hundreds to low thousands of rows, not the audit log's millions) that a
// full-table ILIKE scan is a sub-millisecond cost, not a latency risk. A
// trigram index (`pg_trgm`, `gin (name gin_trgm_ops)`) would make this
// index-servable, but is deliberately NOT added in this slice: enabling a new
// Postgres extension is a deployment/ops decision (an operator must run
// `CREATE EXTENSION`, and a hosted Postgres may restrict which extensions are
// allowlisted at all), not something a query-parameter feature should impose
// as a side effect. It belongs in its own reviewed migration if and when this
// scan is ever measured to matter.
func likeSearchArg(term string) any {
	if term == "" {
		return nil
	}
	return "%" + escapeLikeSpecials(term) + "%"
}

// escapeLikeSpecials escapes the characters that change meaning inside a
// LIKE/ILIKE pattern before likeSearchArg wraps term in the two literal '%'
// wildcards that make it a substring match. Postgres' default LIKE/ILIKE
// escape character is backslash, with no ESCAPE clause needed to select it
// (verified against the PostgreSQL pattern-matching docs), so escaping here
// is exactly the three characters that default applies to: a literal
// backslash first, which must run before the other two, or the backslashes
// this function introduces to escape '%' and '_' would themselves be escaped
// a second time, producing a broken pattern rather than a stricter one; then
// '%' (matches any run of characters) and '_' (matches any single character).
//
// Without this, a user searching for a literal "_" in a name (e.g. hunting
// for "my_server" among catalog entries) would have every row containing
// "my" + any-one-character + "server" match, silently returning far more
// than the exact substring they typed: a small, embarrassing, and very
// findable bug the moment any real name contains either character. '%' in a
// search term would behave the same way, only more so: it wildcards an
// arbitrary RUN of characters rather than exactly one.
func escapeLikeSpecials(term string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(term)
}
