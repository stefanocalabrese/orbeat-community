package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestPaginatedListsUseTheirIndexes is the assertion migration 00012's own test
// (TestIndexSetsAfterFullMigration, internal/migrate/migrate_test.go — Task
// 1b's commit d6ce671 replaced the earlier TestPaginationIndexesExist with
// this) cannot make: that each keyset query actually DRIVES the index built
// for it, not merely that the index exists in the catalog. Pinning the exact
// index-NAME SET from pg_indexes stays green for the entire life of a query
// that ignores one of those indexes — exactly what happened to
// audit_event_tenant_ts_id_idx (migration 00010), which was never usable by the
// query it was built for, for its whole life, because that query's unqualified
// `ORDER BY id` sorted by the projected TEXT LABEL rather than the native uuid
// column (see paging.go's keysetTail doc and Task 2b's audit_test.go). This
// test cannot make that mistake invisible again.
//
// Two assertions, of deliberately different reach — see assertPaginationPlan:
//
//  1. PRIMARY, unconditional: the plan must not contain `(id)::text`. No
//     Postgres index can ever supply an ordering over a text CAST of a uuid
//     column, so a query with the C3 defect must materialize a sort node
//     naming the cast — at any row count, on any Postgres version, regardless
//     of table bloat or which physical-insertion-order/sort-column correlation
//     the seed happens to produce. Verified to separate broken from fixed at
//     n=5 with no ANALYZE at all. This check alone would carry the whole test.
//  2. SECONDARY, seed-dependent: the plan names the index built for this query
//     and (for four of the seven subtests) contains no `Sort Key` at all — i.e.
//     the index supplies the order outright, with no residual sort node. THIS
//     assertion can and does flip with row count, table bloat and statistics
//     (Task 2b measured the index-name/no-Sort-Key pair passing on BOTH sides
//     of a real defect at some seeds) — it is corroborating evidence that the
//     query is actually driven the way production wants, not the proof that
//     the C3 class is fixed. Three of the seven subtests (artifact_revision,
//     role, mcp_server) legitimately produce a Sort Key on correct code — see
//     each subtest's comment — so "no Sort Key" was never going to be a
//     universal gate; only assertion 1 is.
//
// What this test CANNOT prove, on either count:
//   - Nothing about RESULT correctness — no row is ever compared here. That is
//     covered elsewhere (TestEntitlementPageNonUniqueSortKey,
//     TestArtifactEntitlementPageNonUniqueSortKey,
//     TestArtifactRevisionPageDescendingWalk, TestRolePageWalk, …).
//   - Runtime cost or actual timing — this runs a plain EXPLAIN (no ANALYZE),
//     which reports the planner's cost-based CHOICE deterministically from
//     table statistics, not a measured execution. See the explain() doc
//     comment in storetest_test.go for why VERBOSE is deliberately not used
//     either.
//   - That the SAME plan holds at every possible table shape in production —
//     only that the seed shape built here (documented per subtest) elicits an
//     index-driven plan. A sufficiently different shape could tip the planner
//     the other way; that would be a finding about the seed, not evidence the
//     C3 class regressed, precisely because assertion 1 is shape-independent
//     and assertion 2 is not.
//   - Coverage here is ENUMERATIVE, not structural: this file gates exactly
//     the seven call sites it was written to know about. A paginated list
//     added later without its own subtest here is silently ungated — nothing
//     in this package fails until someone remembers to add one.
func TestPaginatedListsUseTheirIndexes(t *testing.T) {
	t.Run("entitlement", testEntitlementPlan)
	t.Run("artifact_entitlement", testArtifactEntitlementPlan)
	t.Run("artifact_unfiltered", testArtifactUnfilteredPlan)
	t.Run("artifact_state_filtered", testArtifactStateFilteredPlan)
	// artifact_revision is Enterprise-only (docs/specs/2026-08-19-orbeat-
	// community-repo-generation-design.md §4) — its subtest,
	// testArtifactRevisionPlan, lives in explain_revisions.ee_test.go and
	// runs as its own top-level test (TestArtifactRevisionPlanUsesItsIndex)
	// rather than through this shared runner.
	t.Run("role", testRolePlan)
	t.Run("mcp_server", testMCPServerPlan)
}

// planExpectation is what a subtest requires of its plan, beyond the
// unconditional `(id)::text`-absence and Seq-Scan-absence checks every subtest
// gets. wantIndex == "" skips the index-name check; noSortKey == false skips
// the Sort-Key-absence check — used by the three lists (artifact_revision,
// role, mcp_server) that legitimately produce a Sort Key on correct code (see
// their subtests for why), where the migration deliberately built no
// purpose-fit index. Those three still set wantIndex: it is not the same
// claim as noSortKey — it pins that the plan actually reaches the intended
// unique index by name, independent of whether anything sorts on top of it.
//
// CAVEAT for the three noSortKey==false lists: wantIndex does NOT catch a
// degraded Bitmap Index Scan. Verified live (see testArtifactRevisionPlan's
// comment): shrinking revisions-per-artifact on that seed from 2000 to 200
// degrades the real plan from `Index Scan Backward` to `Sort → Bitmap Heap
// Scan` — and the index NAME still appears in both, since a Bitmap Index
// Scan also names its index. The signal that WOULD distinguish them (`Index
// Scan Backward using …` / `Presorted Key`) is a specific access-method
// shape one Postgres planner-version choice away from a false failure on
// otherwise-correct code, so this file deliberately does not pin it —
// wantIndex is the shape-independent floor for these three, not a complete
// plan-shape gate.
type planExpectation struct {
	wantIndex string
	noSortKey bool
}

// assertPaginationPlan collects both plan-shape reaches described in this
// file's doc comment into one t.Errorf per call, so a red run shows the whole
// picture at once: the `(id)::text` and Seq-Scan checks apply to every
// subtest unconditionally; wantIndex/noSortKey are opt-in per list. It
// supersedes Task 2b's narrower assertKeysetPlan (formerly in
// audit_test.go, now collapsed into this single helper so the C3 failure
// string lives in exactly one place) — TestAuditPageUsesKeysetIndex's three
// call sites use it directly, with noSortKey always true and wantIndex
// always "audit_event_tenant_ts_id_idx".
func assertPaginationPlan(t *testing.T, plan, what string, exp planExpectation) {
	t.Helper()
	var problems []string
	if strings.Contains(plan, "(id)::text") {
		problems = append(problems, "sorts by the TEXT CAST of id, not the native uuid column — the C3 defect is back (re-qualify the ORDER BY keys — see keysetTail in paging.go)")
	}
	if strings.Contains(plan, "Seq Scan") {
		problems = append(problems, "chose a Seq Scan instead of an index")
	}
	if exp.wantIndex != "" && !strings.Contains(plan, exp.wantIndex) {
		problems = append(problems, fmt.Sprintf("does not use %s", exp.wantIndex))
	}
	if exp.noSortKey && strings.Contains(plan, "Sort Key") {
		problems = append(problems, "still sorts; the index should supply the order")
	}
	if len(problems) > 0 {
		t.Errorf("%s: %s.\nPlan:\n%s", what, strings.Join(problems, "; "), plan)
	}
}

// cleanupTenant deletes tn (and, via every child table's `tenant_id … ON
// DELETE CASCADE REFERENCES tenant(id)`, everything this subtest seeded under
// it — role/mcp_server/entitlement/artifact/artifact_entitlement/
// artifact_revision all cascade this way) then re-ANALYZEs every table named
// in tables so its statistics reflect what is actually left, not the union of
// this subtest's seed and whatever ran before it.
//
// internal/store shares ONE Postgres container across the whole package, and
// plan choice is a function of table-WIDE statistics — Task 2b's own
// TestAuditPageUsesKeysetIndex established this convention, and it matters
// more here: several of these seven subtests share TWO or THREE physical
// tables (e.g. the entitlement subtest also bulk-inserts mcp_server rows to
// satisfy the FK, and four of the seven subtests write into `artifact`), so a
// per-table DELETE list is one forgotten table away from leaking seed rows
// into a sibling subtest's ANALYZE. Deleting the tenant row instead is
// complete by construction — it cannot forget a table, because every table
// that could leak rows is already wired to cascade off tenant_id.
//
// That completeness claim is about the DELETE only. The caller-supplied
// tables list is NOT derived from anything and has no equivalent guarantee —
// it is a plain reminder to the human writing the subtest to name every table
// this subtest wrote into (a stats-only mistake here would not fail loudly;
// it would just make a sibling subtest's plan choice quietly less reliable).
// Each subtest's tables argument is hand-verified against its own seeding
// code, not generated from it.
func cleanupTenant(t *testing.T, s *Store, tenantID string, tables ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := s.db.Exec(ctx, `DELETE FROM tenant WHERE id = $1`, tenantID); err != nil {
			t.Errorf("cleanup: delete tenant %s: %v", tenantID, err)
		}
		for _, table := range tables {
			if _, err := s.db.Exec(ctx, `ANALYZE `+table); err != nil {
				t.Errorf("cleanup: re-analyze %s: %v", table, err)
			}
		}
	})
}

// testEntitlementPlan seeds one role with 2000 entitlements (each against its
// own mcp_server row, to satisfy the FK) — enough for the planner to prefer
// entitlement_tenant_role_id_idx (tenant_id, role_id, id) over a Seq Scan +
// Sort. Bulk-inserted via a data-modifying CTE (mcp_server rows, then the
// entitlements referencing them) rather than 2000 CreateMCPServer/
// CreateEntitlement round trips, for speed.
//
// Calls the real ListEntitlementsPage for the first page (exercising
// entitlementPageSQL's cursor==nil branch as a side effect) purely to obtain a
// deep cursor from its last row; the plan asserted on is built by calling
// entitlementPageSQL directly with that cursor — the exact function
// ListEntitlementsPage itself calls, never a hand-rebuilt equivalent (C7).
func testEntitlementPlan(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	cleanupTenant(t, s, tn.ID, "role", "mcp_server", "entitlement")

	role, err := s.CreateRole(ctx, tn.ID, "plan-entitlement-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	const n = 2000
	if _, err := s.db.Exec(ctx, `
		WITH servers AS (
			INSERT INTO mcp_server (tenant_id, name, transport, endpoint_or_command, status)
			SELECT $1, 'plan-ent-srv-' || g, 'http', 'https://example.invalid/mcp', 'active'
			FROM generate_series(1, $2) AS g
			RETURNING id
		)
		INSERT INTO entitlement (tenant_id, role_id, mcp_server_id)
		SELECT $1, $3, id FROM servers`,
		tn.ID, n, role.ID); err != nil {
		t.Fatalf("seed entitlements: %v", err)
	}
	if _, err := s.db.Exec(ctx, `ANALYZE mcp_server`); err != nil {
		t.Fatalf("analyze mcp_server: %v", err)
	}
	if _, err := s.db.Exec(ctx, `ANALYZE entitlement`); err != nil {
		t.Fatalf("analyze entitlement: %v", err)
	}

	first, err := s.ListEntitlementsPage(ctx, tn.ID, nil, 100)
	if err != nil || len(first) != 100 {
		t.Fatalf("seed page: %d rows, err=%v", len(first), err)
	}
	cursor := EntitlementCursor(first[len(first)-1])
	sql, args, err := entitlementPageSQL(tn.ID, &cursor, 100)
	if err != nil {
		t.Fatalf("entitlementPageSQL: %v", err)
	}
	plan := explain(t, s, sql, args...)
	assertPaginationPlan(t, plan, "entitlement", planExpectation{
		wantIndex: "entitlement_tenant_role_id_idx",
		noSortKey: true,
	})
}

// testArtifactEntitlementPlan mirrors testEntitlementPlan for the second
// non-unique-role_id list: one role, 2000 role-visibility artifacts, one
// artifact_entitlement per artifact — enough for the planner to prefer
// artifact_entitlement_tenant_role_id_idx over a Seq Scan + Sort.
func testArtifactEntitlementPlan(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	cleanupTenant(t, s, tn.ID, "artifact", "artifact_entitlement", "role")

	role, err := s.CreateRole(ctx, tn.ID, "plan-ae-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	const n = 2000
	if _, err := s.db.Exec(ctx, `
		WITH arts AS (
			INSERT INTO artifact (tenant_id, type, name, content, visibility)
			SELECT $1, 'skill', 'plan-ae-art-' || g, 'body', 'role'
			FROM generate_series(1, $2) AS g
			RETURNING id
		)
		INSERT INTO artifact_entitlement (tenant_id, role_id, artifact_id)
		SELECT $1, $3, id FROM arts`,
		tn.ID, n, role.ID); err != nil {
		t.Fatalf("seed artifact entitlements: %v", err)
	}
	if _, err := s.db.Exec(ctx, `ANALYZE artifact`); err != nil {
		t.Fatalf("analyze artifact: %v", err)
	}
	if _, err := s.db.Exec(ctx, `ANALYZE artifact_entitlement`); err != nil {
		t.Fatalf("analyze artifact_entitlement: %v", err)
	}

	first, err := s.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 100)
	if err != nil || len(first) != 100 {
		t.Fatalf("seed page: %d rows, err=%v", len(first), err)
	}
	cursor := ArtifactEntitlementCursor(first[len(first)-1])
	sql, args, err := artifactEntitlementPageSQL(tn.ID, &cursor, 100)
	if err != nil {
		t.Fatalf("artifactEntitlementPageSQL: %v", err)
	}
	plan := explain(t, s, sql, args...)
	assertPaginationPlan(t, plan, "artifact_entitlement", planExpectation{
		wantIndex: "artifact_entitlement_tenant_role_id_idx",
		noSortKey: true,
	})
}

// testArtifactUnfilteredPlan seeds 2000 artifacts (no approval-state filter:
// ArtifactPageOpts.State == ""), enough for the planner to prefer
// artifact_tenant_type_name_id_idx (tenant_id, type, name, id) at cursor depth
// over the UNIQUE(tenant_id, type, name) constraint's index, which migration
// 00012 documents as only a shallow-cursor path.
func testArtifactUnfilteredPlan(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	cleanupTenant(t, s, tn.ID, "artifact")

	const n = 2000
	if _, err := s.db.Exec(ctx, `
		INSERT INTO artifact (tenant_id, type, name, content)
		SELECT $1, 'skill', 'plan-art-' || g, 'body'
		FROM generate_series(1, $2) AS g`, tn.ID, n); err != nil {
		t.Fatalf("seed artifacts: %v", err)
	}
	if _, err := s.db.Exec(ctx, `ANALYZE artifact`); err != nil {
		t.Fatalf("analyze artifact: %v", err)
	}

	opts := ArtifactPageOpts{Limit: 100}
	first, err := s.ListArtifactsPage(ctx, tn.ID, opts)
	if err != nil || len(first) != 100 {
		t.Fatalf("seed page: %d rows, err=%v", len(first), err)
	}
	cursor := ArtifactCursor(first[len(first)-1])
	opts.Cursor = &cursor
	sql, args, err := artifactPageSQL(tn.ID, opts)
	if err != nil {
		t.Fatalf("artifactPageSQL: %v", err)
	}
	plan := explain(t, s, sql, args...)
	assertPaginationPlan(t, plan, "artifact (unfiltered)", planExpectation{
		wantIndex: "artifact_tenant_type_name_id_idx",
		noSortKey: true,
	})
}

// testArtifactStateFilteredPlan seeds 2000 artifacts, ~25% 'pending' and the
// rest 'draft', then pages with State: "pending" — exercising
// artifactPageSQL's `$2::text IS NULL OR approval_state = $2` guard with a
// REAL non-NULL value, which (per artifact.go's own comment on that guard)
// constant-folds under Postgres' custom plan to a plain equality and makes
// artifact_tenant_state_type_name_id_idx (tenant_id, approval_state, type,
// name, id) available. Runs in its own tenant from testArtifactUnfilteredPlan,
// so cleanupTenant's per-subtest re-ANALYZE (not a shared one at the end of
// the parent test) is what keeps the two from polluting each other's
// statistics — see cleanupTenant's doc comment.
func testArtifactStateFilteredPlan(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	cleanupTenant(t, s, tn.ID, "artifact")

	const n = 2000
	if _, err := s.db.Exec(ctx, `
		INSERT INTO artifact (tenant_id, type, name, content, approval_state)
		SELECT $1, 'skill', 'plan-art-state-' || g, 'body',
		       CASE WHEN g % 4 = 0 THEN 'pending' ELSE 'draft' END
		FROM generate_series(1, $2) AS g`, tn.ID, n); err != nil {
		t.Fatalf("seed artifacts: %v", err)
	}
	if _, err := s.db.Exec(ctx, `ANALYZE artifact`); err != nil {
		t.Fatalf("analyze artifact: %v", err)
	}

	opts := ArtifactPageOpts{State: "pending", Limit: 100}
	first, err := s.ListArtifactsPage(ctx, tn.ID, opts)
	if err != nil || len(first) != 100 {
		t.Fatalf("seed page: %d rows, err=%v", len(first), err)
	}
	cursor := ArtifactCursor(first[len(first)-1])
	opts.Cursor = &cursor
	sql, args, err := artifactPageSQL(tn.ID, opts)
	if err != nil {
		t.Fatalf("artifactPageSQL: %v", err)
	}
	plan := explain(t, s, sql, args...)
	assertPaginationPlan(t, plan, "artifact (state= filtered)", planExpectation{
		wantIndex: "artifact_tenant_state_type_name_id_idx",
		noSortKey: true,
	})
}

// testArtifactRevisionPlan moved to explain_revisions.ee_test.go: revision
// pagination is Enterprise-only (docs/specs/2026-08-19-orbeat-community-
// repo-generation-design.md §4).

// testRolePlan seeds 2000 roles. Migration 00012 deliberately built no
// purpose-fit index for this list — it rides UNIQUE(tenant_id, name)'s
// auto-generated index — so, per the migration's own rationale (arity: one
// extra sort column past tenant_id gives the planner a clean scalar bound to
// seek on) a Sort Key on id is expected and acceptable; wantIndex is still
// asserted (see planExpectation's caveat for what it does and does not
// catch). The `(id)::text` check still applies unconditionally: id is
// projected `id::text` exactly like every other list, so a permitted Sort
// Key on the native `name` column is not permission for one on
// `((id)::text)` — that would still be the C3 defect.
func testRolePlan(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	cleanupTenant(t, s, tn.ID, "role")

	const n = 2000
	if _, err := s.db.Exec(ctx, `
		INSERT INTO role (tenant_id, name)
		SELECT $1, 'plan-role-' || g
		FROM generate_series(1, $2) AS g`, tn.ID, n); err != nil {
		t.Fatalf("seed roles: %v", err)
	}
	if _, err := s.db.Exec(ctx, `ANALYZE role`); err != nil {
		t.Fatalf("analyze role: %v", err)
	}

	first, err := s.ListRolesPage(ctx, tn.ID, nil, 100)
	if err != nil || len(first) != 100 {
		t.Fatalf("seed page: %d rows, err=%v", len(first), err)
	}
	cursor := RoleCursor(first[len(first)-1])
	sql, args, err := rolePageSQL(tn.ID, &cursor, 100)
	if err != nil {
		t.Fatalf("rolePageSQL: %v", err)
	}
	plan := explain(t, s, sql, args...)
	assertPaginationPlan(t, plan, "role", planExpectation{
		wantIndex: "role_tenant_id_name_key",
	})
}

// testMCPServerPlan mirrors testRolePlan: mcp_server also rides
// UNIQUE(tenant_id, name)'s auto-generated index by deliberate design (00012),
// with the same "Sort Key on id is fine, Seq Scan is not, wantIndex still
// pinned" exemption.
func testMCPServerPlan(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	cleanupTenant(t, s, tn.ID, "mcp_server")

	const n = 2000
	if _, err := s.db.Exec(ctx, `
		INSERT INTO mcp_server (tenant_id, name, transport, endpoint_or_command, status)
		SELECT $1, 'plan-srv-' || g, 'http', 'https://example.invalid/mcp', 'active'
		FROM generate_series(1, $2) AS g`, tn.ID, n); err != nil {
		t.Fatalf("seed mcp servers: %v", err)
	}
	if _, err := s.db.Exec(ctx, `ANALYZE mcp_server`); err != nil {
		t.Fatalf("analyze mcp_server: %v", err)
	}

	first, err := s.ListMCPServersPage(ctx, tn.ID, nil, 100)
	if err != nil || len(first) != 100 {
		t.Fatalf("seed page: %d rows, err=%v", len(first), err)
	}
	cursor := MCPServerCursor(first[len(first)-1])
	sql, args, err := mcpServerPageSQL(tn.ID, &cursor, 100)
	if err != nil {
		t.Fatalf("mcpServerPageSQL: %v", err)
	}
	plan := explain(t, s, sql, args...)
	assertPaginationPlan(t, plan, "mcp_server", planExpectation{
		wantIndex: "mcp_server_tenant_id_name_key",
	})
}
