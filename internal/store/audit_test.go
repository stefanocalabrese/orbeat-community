package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestListAuditEventsPageKeyset(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	for i := 0; i < 5; i++ {
		if _, err := s.AppendAuditEvent(ctx, AuditEvent{TenantID: tn.ID, Actor: "a", Action: "x", Decision: "allow"}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	page1, err := s.ListAuditEventsPage(ctx, tn.ID, nil, 2)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	last := page1[len(page1)-1]
	page2, err := s.ListAuditEventsPage(ctx, tn.ID, &AuditCursor{TS: last.TS, ID: last.ID}, 2)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}
	for _, a := range page1 {
		for _, b := range page2 {
			if a.ID == b.ID {
				t.Fatalf("pages overlap on id %s", a.ID)
			}
		}
	}
}

func TestListAuditEventsPageEqualTimestamps(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	// All four rows share one ts: now() is constant within a single transaction.
	if err := s.InTx(ctx, func(tx *Store) error {
		for i := 0; i < 4; i++ {
			if _, e := tx.AppendAuditEvent(ctx, AuditEvent{TenantID: tn.ID, Actor: "a", Action: "x", Decision: "allow"}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Sanity: confirm the rows really do share a timestamp (otherwise this test
	// would not exercise the (ts=, id<) tiebreak it exists to cover).
	all, err := s.ListAuditEventsPage(ctx, tn.ID, nil, 10)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("seeded len = %d, want 4", len(all))
	}
	if !all[0].TS.Equal(all[3].TS) {
		t.Fatalf("expected equal timestamps, got %v vs %v", all[0].TS, all[3].TS)
	}

	// Page through in steps of 2 using the keyset cursor; collect ids.
	seen := map[string]int{}
	var cursor *AuditCursor
	for {
		page, err := s.ListAuditEventsPage(ctx, tn.ID, cursor, 2)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			seen[e.ID]++
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		cursor = &AuditCursor{TS: last.TS, ID: last.ID}
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 distinct ids across pages, got %d: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("id %s appeared %d times (overlap/skip)", id, n)
		}
	}
}

func TestAuditAppendAndList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	_, err := s.AppendAuditEvent(ctx, AuditEvent{
		TenantID: tn.ID,
		Actor:    "alice",
		Action:   "tool_call",
		Target:   "github/list_repos",
		Decision: "allow",
		Metadata: map[string]any{"latency_ms": 12},
	})
	if err != nil {
		t.Fatalf("AppendAuditEvent: %v", err)
	}
	_, err = s.AppendAuditEvent(ctx, AuditEvent{
		TenantID: tn.ID,
		Actor:    "bob",
		Action:   "tool_call",
		Target:   "github/delete_repo",
		Decision: "deny",
	})
	if err != nil {
		t.Fatalf("AppendAuditEvent(2): %v", err)
	}

	events, err := s.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if err != nil {
		t.Fatalf("ListAuditEventsByTenant: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	// Ordering invariant: ORDER BY ts DESC means timestamps are non-increasing,
	// for whatever values the DB assigned (robust to equal-ts ties).
	if events[0].TS.Before(events[1].TS) {
		t.Fatalf("events not newest-first: %v before %v", events[0].TS, events[1].TS)
	}
	// Content round-trips, asserted order-independently.
	byActor := make(map[string]AuditEvent, len(events))
	for _, e := range events {
		byActor[e.Actor] = e
	}
	if got := byActor["alice"]; got.Decision != "allow" || got.Target != "github/list_repos" {
		t.Fatalf("alice event = %+v", got)
	}
	if got := byActor["bob"]; got.Decision != "deny" || got.Target != "github/delete_repo" {
		t.Fatalf("bob event = %+v", got)
	}
}

func TestListAuditEventsInRange(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	e1, _ := st.AppendAuditEvent(ctx, AuditEvent{TenantID: tn.ID, Actor: "a1", Action: "x", Target: "t", Decision: "allow"})
	e2, _ := st.AppendAuditEvent(ctx, AuditEvent{TenantID: tn.ID, Actor: "a2", Action: "y", Target: "t", Decision: "deny"})
	e3, _ := st.AppendAuditEvent(ctx, AuditEvent{TenantID: tn.ID, Actor: "a3", Action: "z", Target: "t", Decision: "allow"})

	// no bounds → all 3, ascending by ts
	all, err := st.ListAuditEventsInRange(ctx, tn.ID, nil, nil, 1000)
	if err != nil || len(all) != 3 {
		t.Fatalf("all: err=%v n=%d", err, len(all))
	}
	if all[0].ID != e1.ID || all[2].ID != e3.ID {
		t.Fatalf("want ascending by ts (e1..e3), got %s..%s", all[0].ID, all[2].ID)
	}

	// from = e2.TS → excludes e1
	from := e2.TS
	got, err := st.ListAuditEventsInRange(ctx, tn.ID, &from, nil, 1000)
	if err != nil {
		t.Fatalf("from: %v", err)
	}
	for _, e := range got {
		if e.ID == e1.ID {
			t.Fatalf("from filter must exclude e1")
		}
	}
	if len(got) != 2 {
		t.Fatalf("want 2 from e2, got %d", len(got))
	}

	// to = e2.TS → excludes e3
	to := e2.TS
	got, err = st.ListAuditEventsInRange(ctx, tn.ID, nil, &to, 1000)
	if err != nil {
		t.Fatalf("to: %v", err)
	}
	for _, e := range got {
		if e.ID == e3.ID {
			t.Fatalf("to filter must exclude e3")
		}
	}
	if len(got) != 2 {
		t.Fatalf("to filter: want 2 (e1,e2), got %d", len(got))
	}

	// limit caps
	capped, _ := st.ListAuditEventsInRange(ctx, tn.ID, nil, nil, 2)
	if len(capped) != 2 {
		t.Fatalf("limit cap: want 2, got %d", len(capped))
	}

	// tenant isolation
	other := mustTenant(t, st)
	iso, _ := st.ListAuditEventsInRange(ctx, other.ID, nil, nil, 1000)
	if len(iso) != 0 {
		t.Fatalf("tenant isolation: want 0, got %d", len(iso))
	}
}

func TestPruneAuditEventsOlderThan(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	// Insert rows at known ages via raw SQL (AppendAuditEvent would stamp now()).
	insert := func(age time.Duration) {
		_, err := s.db.Exec(ctx, `
			INSERT INTO audit_event (tenant_id, ts, actor, action, target, decision, metadata)
			VALUES ($1, now() - $2::interval, 'tester', 'read', '', 'allow', '{}')`,
			tn.ID, fmt.Sprintf("%d seconds", int(age.Seconds())))
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		insert(100 * 24 * time.Hour) // 100 days old — must be pruned
	}
	insert(1 * 24 * time.Hour) // 1 day old — must survive

	cutoff := time.Now().Add(-30 * 24 * time.Hour)              // keep rows younger than 30 days
	deleted, err := s.PruneAuditEventsOlderThan(ctx, cutoff, 2) // batch 2 < 5 backlog → exercises the loop
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	// Prune is cross-tenant global (no tenant filter), and the store package shares
	// one DB across tests — so `deleted` counts every old row, not just ours. Assert
	// our 5 were among them (>=); the tenant-scoped `remaining` below is the strict check.
	if deleted < 5 {
		t.Errorf("deleted = %d, want >= 5 (our five 100-day-old rows)", deleted)
	}

	var remaining int
	if err := s.db.QueryRow(ctx,
		`SELECT count(*) FROM audit_event WHERE tenant_id = $1`, tn.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Errorf("remaining = %d, want 1 (the 1-day-old row)", remaining)
	}
}

// TestAuditPageUsesKeysetIndex pins that the id tiebreak the audit page's
// projection casts to text (`SELECT id::text …`) never leaks into the plan as
// a text-typed sort — the exact shape correction C3 describes — and,
// separately, that audit_event_tenant_ts_id_idx (the index migration 00010
// added for exactly this query, whose comment says it exists so "the id
// tiebreak fell out of the index and every page did an extra sort") is
// actually DRIVEN, with no residual sort node at all, once the query is run
// against a shape where that's decidable.
//
// It checks THREE production call sites, not one: both branches of
// auditPageSQL (cursor == nil, the most-executed one since it serves every
// first page, and cursor != nil) and auditRangeSelect (the export path). An
// earlier version of this test only EXPLAINed the cursor != nil branch — a
// silent gap, proven by reverting the fix on JUST the other two sites and
// running the suite: it stayed green, because nothing exercised them. All
// three funnel through the identical `id::text` projection / bare `ORDER BY
// id` shape, so a fix landing on only one is exactly the kind of partial fix
// this task's own house rule ("a fix for a defect class must end with a grep
// for the class") exists to catch — the grep found the sites; only asserting
// against all of them proves each one actually got the fix, not just the one
// this test happened to call first.
//
// Two DISTINCT assertions per site, because they have different reach:
//
//  1. The plan must not contain `(id)::text`. This is unconditionally
//     guaranteed, at every table shape and row count: no index can serve a
//     text-cast ordering over a uuid column, so Postgres MUST materialize a
//     sort node naming the cast whenever the unqualified query runs, and
//     never does once table-qualification makes `id` resolve to the native
//     uuid column instead of the output label. Verified across single-tenant
//     seeds from 500 to 100,000 rows, both directions of ts/insertion-order
//     correlation, and the noise-tenant seed below — always fires, in both
//     directions (present pre-fix, absent post-fix), at all three sites.
//
//  2. The plan must name audit_event_tenant_ts_id_idx and contain no `Sort
//     Key` line at all (not merely an untyped one). This is NOT
//     unconditionally decidable — see the seeding comment below — which is
//     why assertion 1 exists as the one that always carries the test. If
//     assertion 2 fires on an otherwise-correct fix, the more likely
//     explanation is the SEED SHAPE moved (e.g. this file's own tests, which
//     share one package-wide table, seeded very differently), not a query
//     regression — check assertion 1's verdict first.
//
// Seeding needs care, and the obvious approach is misleading (verified
// empirically over dozens of seeded runs, not assumed). A single dominant
// tenant — the real production shape, since orbeat is deployed
// single-tenant-per-instance — makes assertion 2 UNRELIABLE, not because
// tenant_id filtering is cheap (that's true but incomplete), but because
// whether Postgres' cost model prefers Incremental-Sort-over-audit_event_ts_idx
// or a clean Index-Scan-over-the-composite-index at that shape is close to a
// coin flip, and the direction of correlation between ts and physical
// insertion order shifts which side of that near-tie it lands on: bulk-seeded
// with ts running the SAME direction as insertion order (the natural,
// chronological pattern real audit-event appends actually have), the fixed
// query still chose the sort-based plan in 10 of 13 sampled runs across
// 500-100,000 rows — assertion 2 would be red-flaky even after the fix is
// correct. A second "noise" tenant interleaved 9:1 with the target tenant
// removes the ambiguity: tenant_id becomes genuinely selective (mirroring how
// role_id is genuinely selective in the unrelated entitlement measurement —
// see paging.go), so a targeted composite-index seek is unambiguously cheaper
// than scanning-then-filtering, REGARDLESS of ts/insertion-order correlation
// direction — reproduced stable (5/5) at both directions, unlike either
// single-tenant shape alone.
func TestAuditPageUsesKeysetIndex(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	noise := mustTenant(t, s)

	// total=20000 (2000 target rows interleaved 1-in-10 with 18000 noise
	// rows) is the smallest volume found to give a stable, meaningful plan on
	// both sides of the fix: RED shows an Incremental Sort layered on TOP of
	// audit_event_tenant_ts_id_idx (Sort Key: ts DESC, ((id)::text) DESC);
	// GREEN shows a bare Index Scan with no Sort node at all. Bulk-inserted
	// via SQL rather than 20000 AppendAuditEvent round trips for speed.
	//
	// `now() - g microseconds` makes ts DESCEND as g — and physical insertion
	// order — ASCEND: anti-correlated with a real append-only audit log,
	// where ts and insertion order move together. This is the ONE direction
	// verified stable (12/12 sampled reps) at single-tenant volume alone;
	// the noise-tenant interleaving above makes the choice of direction moot
	// for THIS test (reproduced stable 5/5 at the natural/chronological
	// direction too), but the seed ships with the direction that was
	// independently confirmed most favorable, not the realistic one.
	const total = 20000
	if _, err := s.db.Exec(ctx, `
		INSERT INTO audit_event (tenant_id, ts, actor, action, target, decision)
		SELECT CASE WHEN g % 10 = 0 THEN $1::uuid ELSE $2::uuid END,
		       now() - (g || ' microseconds')::interval,
		       'a@example.com', 'test.plan', 't', 'allow'
		FROM generate_series(1, $3) AS g`, tn.ID, noise.ID, total); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.db.Exec(ctx, `ANALYZE audit_event`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// Plan choice is a function of table-WIDE statistics, not just this
	// test's own rows: the package shares one Postgres container, and this
	// test's 20000-row ANALYZE would otherwise persist as background noise
	// for every subsequent test's plan decisions on this table (no live
	// defect today — see the task's own commit message for why — but the
	// convention matters for tests added later, e.g. Task 2c's two subtests
	// sharing the artifact table). Clean up what this test added, then
	// re-ANALYZE so the table's statistics reflect what's actually left.
	t.Cleanup(func() {
		cctx := context.Background()
		if _, err := s.db.Exec(cctx, `DELETE FROM audit_event WHERE tenant_id IN ($1,$2)`, tn.ID, noise.ID); err != nil {
			t.Errorf("cleanup: delete seeded audit_event rows: %v", err)
		}
		if _, err := s.db.Exec(cctx, `ANALYZE audit_event`); err != nil {
			t.Errorf("cleanup: re-analyze audit_event: %v", err)
		}
	})

	// Site 1: cursor != nil branch. Page 2 of a 2000-row/tenant table is
	// "deep" only relative to a single 100-row page, not to the keyset's
	// full history — deep enough that a plan serving page 1 alone couldn't
	// be mistaken for one serving this page too.
	first, err := s.ListAuditEventsPage(ctx, tn.ID, nil, 100)
	if err != nil || len(first) != 100 {
		t.Fatalf("seed page: %d rows, err=%v", len(first), err)
	}
	cursor := &AuditCursor{TS: first[len(first)-1].TS, ID: first[len(first)-1].ID}
	cursorSQL, cursorArgs := auditPageSQL(tn.ID, cursor, 100)
	auditExp := planExpectation{wantIndex: "audit_event_tenant_ts_id_idx", noSortKey: true}
	assertPaginationPlan(t, explain(t, s, cursorSQL, cursorArgs...), "cursor page (ListAuditEventsPage, cursor != nil)", auditExp)

	// Site 2: cursor == nil branch — every first page a client ever sees,
	// and the one a prior version of this test never EXPLAINed at all.
	nilSQL, nilArgs := auditPageSQL(tn.ID, nil, 100)
	assertPaginationPlan(t, explain(t, s, nilSQL, nilArgs...), "first page (ListAuditEventsPage, cursor == nil)", auditExp)

	// Site 3: auditRangeSelect — the audit export path
	// (ForEachAuditEventInRange / ListAuditEventsInRange), unbounded [from,
	// to] so it walks the same rows as the other two sites.
	assertPaginationPlan(t, explain(t, s, auditRangeSelect, tn.ID, nil, nil, 1000), "range select (auditRangeSelect, export path)", auditExp)
}

func TestAuditRejectsBadDecision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)
	_, err := s.AppendAuditEvent(ctx, AuditEvent{
		TenantID: tn.ID, Actor: "x", Action: "y", Decision: "maybe",
	})
	if err == nil {
		t.Fatal("expected CHECK constraint to reject invalid decision")
	}
}
