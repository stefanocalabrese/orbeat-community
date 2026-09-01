package store

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"testing"
)

// TestListAuditEventsPageFilters covers the actor/action/decision narrowing
// added for the admin console's audit filters. Each subtest asserts BOTH what
// comes back and what does not: a filter that returned every row would satisfy
// a "contains the expected event" assertion just as well as a working one.
func TestListAuditEventsPageFilters(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	seed := []AuditEvent{
		{Actor: "alice", Action: "role.delete", Decision: "allow"},
		{Actor: "alice", Action: "artifact.approve", Decision: "deny"},
		{Actor: "bob", Action: "role.delete", Decision: "deny"},
		{Actor: "bob", Action: "artifact.approve", Decision: "allow"},
		{Actor: "bob", Action: "artifact.approve", Decision: "error"},
	}
	for _, e := range seed {
		e.TenantID = tn.ID
		if _, err := s.AppendAuditEvent(ctx, e); err != nil {
			t.Fatalf("seed %+v: %v", e, err)
		}
	}

	for _, tc := range []struct {
		name   string
		filter AuditFilter
		want   int
	}{
		{"unfiltered", AuditFilter{}, 5},
		{"actor", AuditFilter{Actor: "alice"}, 2},
		{"action", AuditFilter{Action: "role.delete"}, 2},
		{"decision", AuditFilter{Decision: "deny"}, 2},
		{"decision error is a real value", AuditFilter{Decision: "error"}, 1},
		{"actor and decision", AuditFilter{Actor: "bob", Decision: "allow"}, 1},
		{"all three", AuditFilter{Actor: "alice", Action: "role.delete", Decision: "allow"}, 1},
		{"contradictory combination", AuditFilter{Actor: "alice", Action: "role.delete", Decision: "deny"}, 0},
		{"unknown actor", AuditFilter{Actor: "nobody"}, 0},
		{"actor is exact, not a prefix", AuditFilter{Actor: "ali"}, 0},
		{"action is exact, not a prefix", AuditFilter{Action: "role."}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListAuditEventsPage(ctx, tn.ID, tc.filter, nil, 100)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("filter %+v returned %d events, want %d", tc.filter, len(got), tc.want)
			}
			for _, e := range got {
				if tc.filter.Actor != "" && e.Actor != tc.filter.Actor {
					t.Errorf("row %s has actor %q, filter asked for %q", e.ID, e.Actor, tc.filter.Actor)
				}
				if tc.filter.Action != "" && e.Action != tc.filter.Action {
					t.Errorf("row %s has action %q, filter asked for %q", e.ID, e.Action, tc.filter.Action)
				}
				if tc.filter.Decision != "" && e.Decision != tc.filter.Decision {
					t.Errorf("row %s has decision %q, filter asked for %q", e.ID, e.Decision, tc.filter.Decision)
				}
			}
		})
	}
}

// TestListAuditEventsPageFilterPaginates walks a filtered set one row at a
// time. The cursor predicate and the filter predicates share a WHERE clause,
// so a bind-order mistake between them would surface here as a duplicated or
// skipped row rather than as a wrong-looking single page.
func TestListAuditEventsPageFilterPaginates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	const wanted = 3
	for i := 0; i < wanted; i++ {
		if _, err := s.AppendAuditEvent(ctx, AuditEvent{
			TenantID: tn.ID, Actor: "alice", Action: "role.delete", Decision: "deny"}); err != nil {
			t.Fatalf("seed match: %v", err)
		}
		// Interleave non-matching rows so a filter dropped on the second page
		// shows up as an extra row rather than as a silent no-op.
		if _, err := s.AppendAuditEvent(ctx, AuditEvent{
			TenantID: tn.ID, Actor: "bob", Action: "artifact.approve", Decision: "allow"}); err != nil {
			t.Fatalf("seed noise: %v", err)
		}
	}

	filter := AuditFilter{Actor: "alice", Decision: "deny"}
	seen := map[string]bool{}
	var cursor *AuditCursor
	for page := 0; page < wanted+1; page++ {
		got, err := s.ListAuditEventsPage(ctx, tn.ID, filter, cursor, 1)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(got) == 0 {
			break
		}
		e := got[0]
		if e.Actor != "alice" || e.Decision != "deny" {
			t.Fatalf("page %d leaked a non-matching row: actor=%q decision=%q", page, e.Actor, e.Decision)
		}
		if seen[e.ID] {
			t.Fatalf("page %d repeated event %s", page, e.ID)
		}
		seen[e.ID] = true
		cursor = &AuditCursor{TS: e.TS, ID: e.ID}
	}
	if len(seen) != wanted {
		t.Fatalf("walked %d matching events, want %d", len(seen), wanted)
	}
}

// TestAuditFilterPlansUseTheirIndexes pins that a filtered audit page actually
// drives migration 00023's indexes, which is a different claim from
// TestIndexSetsAfterFullMigration's (that they exist) and from
// TestListAuditEventsPageFilters' (that the rows are right). A filter that
// seq-scans returns exactly the same rows, so nothing else in the suite can
// tell the two apart.
//
// The seed is 20,000 events in one tenant with a rare actor and a rare action
// appearing 5 times each. Scale is load-bearing in BOTH directions: too few
// rows and Postgres correctly prefers a seq scan even with the index, which
// would fail on correct code; too selective a distribution and the unindexed
// path never seq-scans, which would pass on the missing index. 00023's own
// comment records the measurement this seed is a cheaper stand-in for.
//
// decision is asserted too, with the OPPOSITE expectation: no index of its own
// (00023 deliberately built none) but still no seq scan, because walking
// audit_event_ts_idx in ts order and filtering finds a page of a 1-in-3 value
// immediately. That subtest is what would notice if someone "fixed" the
// missing decision index by making the decision filter scan the table.
func TestAuditFilterPlansUseTheirIndexes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	cleanupTenant(t, s, tn.ID, "audit_event")

	if _, err := s.db.Exec(ctx, `
INSERT INTO audit_event (tenant_id, ts, actor, action, target, decision, metadata)
SELECT $1,
       now() - (g || ' seconds')::interval,
       'admin-' || (g % 20),
       'act.' || (g % 30),
       'tgt',
       CASE WHEN g % 100 = 0 THEN 'error' WHEN g % 11 = 0 THEN 'deny' ELSE 'allow' END,
       '{}'::jsonb
FROM generate_series(1, 20000) g`, tn.ID); err != nil {
		t.Fatalf("seed bulk: %v", err)
	}
	if _, err := s.db.Exec(ctx, `
INSERT INTO audit_event (tenant_id, ts, actor, action, target, decision, metadata)
SELECT $1, now() - (g*3300 || ' seconds')::interval, 'rare-actor', 'act.rare', 'tgt', 'deny', '{}'::jsonb
FROM generate_series(1, 5) g`, tn.ID); err != nil {
		t.Fatalf("seed rare: %v", err)
	}
	if _, err := s.db.Exec(ctx, `ANALYZE audit_event`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	for _, tc := range []struct {
		name   string
		filter AuditFilter
		exp    planExpectation
	}{
		{"rare actor", AuditFilter{Actor: "rare-actor"}, planExpectation{wantIndex: "audit_event_tenant_actor_ts_id_idx"}},
		{"rare action", AuditFilter{Action: "act.rare"}, planExpectation{wantIndex: "audit_event_tenant_action_ts_id_idx"}},
		{"decision only, deliberately unindexed", AuditFilter{Decision: "error"}, planExpectation{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := auditPageSQL(tn.ID, tc.filter, nil, 100)
			assertPaginationPlan(t, explain(t, s, sql, args...), "filtered audit page ("+tc.name+")", tc.exp)
		})
	}
}

// TestAuditDecisionsMatchTheSchemaCheck derives the decision domain from the
// live CHECK constraint and compares it with AuditDecisions(), which the API
// uses to refuse an unknown ?decision=. Without this, the two drift silently
// and in either direction: a migration adding a fourth value would make the
// API reject rows that exist, and a migration removing one would make it
// accept a filter that can never match. Both failures look like an empty page.
//
// The whole point is that the expectation is READ, not written down here. A
// literal list in this test would be a third copy of the same claim, and the
// two that already exist are the ones that must agree.
func TestAuditDecisionsMatchTheSchemaCheck(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	var def string
	err := s.db.QueryRow(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'audit_event'
		  AND c.contype = 'c'
		  AND pg_get_constraintdef(c.oid) LIKE '%decision%'`).Scan(&def)
	if err != nil {
		t.Fatalf("read the audit_event decision CHECK: %v", err)
	}

	lit := regexp.MustCompile(`'([^']+)'::text`)
	var fromSchema []string
	for _, m := range lit.FindAllStringSubmatch(def, -1) {
		fromSchema = append(fromSchema, m[1])
	}
	if len(fromSchema) == 0 {
		t.Fatalf("parsed no values out of the constraint %q — this test cannot fail as written", def)
	}

	got := AuditDecisions()
	sort.Strings(got)
	sort.Strings(fromSchema)
	if !slices.Equal(got, fromSchema) {
		t.Fatalf("AuditDecisions() = %v, but audit_event.decision accepts %v (constraint: %s)", got, fromSchema, def)
	}
}
