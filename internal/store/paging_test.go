package store

import (
	"context"
	"strings"
	"testing"
)

// TestEntitlementPageNonUniqueSortKey is the decisive test for spec §4.2.
// entitlement's ORDER BY key is role_id, which is NOT unique (one role has many
// entitlements). Paging on a non-unique key silently skips and duplicates rows
// across page boundaries. Walking the whole table one row at a time must yield
// every id exactly once.
//
// RED-PROOF: drop `id` from ListCursor/keysetTail and this test fails with
// duplicates and missing ids. If it passes with an id-less cursor, it is not
// evidence of anything — fix the test, not the code.
func TestEntitlementPageNonUniqueSortKey(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	// One role, five entitlements: every row shares the sort key.
	role, err := s.CreateRole(ctx, tn.ID, "paging-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		srv, err := s.CreateMCPServer(ctx, MCPServer{
			TenantID: tn.ID, Name: seqName("paging-srv", i), Transport: "http",
			EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
		})
		if err != nil {
			t.Fatalf("create server %d: %v", i, err)
		}
		e, err := s.CreateEntitlement(ctx, Entitlement{
			TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID,
		})
		if err != nil {
			t.Fatalf("create entitlement %d: %v", i, err)
		}
		want[e.ID] = true
	}

	seen := map[string]int{}
	var cursor *ListCursor
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("pagination did not terminate after 20 pages of limit=1 over 5 rows")
		}
		page, err := s.ListEntitlementsPage(ctx, tn.ID, cursor, 1)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			seen[e.ID]++
		}
		last := page[len(page)-1]
		c := EntitlementCursor(last)
		cursor = &c
	}

	for id := range want {
		switch seen[id] {
		case 1: // exactly once — correct
		case 0:
			t.Errorf("entitlement %s was SKIPPED across a page boundary", id)
		default:
			t.Errorf("entitlement %s was returned %d times (duplicated across pages)", id, seen[id])
		}
	}
	for id := range seen {
		if !want[id] {
			t.Errorf("unexpected entitlement %s in results", id)
		}
	}
}

// TestListEntitlementsPageUnboundedReturnsEverything pins the shared
// `limit<=0 → LIMIT NULL` contract (keysetTail, this package's paging.go) via
// ListEntitlementsPage directly. ListEntitlementsByTenant, the wrapper this
// used to test, was deleted: its only production caller moved onto
// ListEntitlementsPage(cursor, limit) (Task 7), and no other real caller
// existed (the gateway resolves entitlements via the distinct
// ListEntitlementsByRoles). See
// TestListMCPServersByTenantUnboundedReturnsEverything below for the sibling
// wrapper that DOES still need this shared contract gated through it
// directly.
//
// n is 501 — one past maxListLimit (internal/api/paging.go, 500): a cap at or
// below 500 fails this test, a cap of 502+ would not. Seeded via a single
// bulk INSERT rather than n CreateMCPServer/CreateEntitlement round trips
// (cost: low tens of milliseconds).
func TestListEntitlementsPageUnboundedReturnsEverything(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "full-set-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	const n = 501
	if _, err := s.db.Exec(ctx, `
		WITH new_servers AS (
			INSERT INTO mcp_server (tenant_id, name, transport, endpoint_or_command, status)
			SELECT $1, 'full-set-srv-' || g, 'http', 'https://example.invalid/mcp', 'active'
			FROM generate_series(1, $2) AS g
			RETURNING id
		)
		INSERT INTO entitlement (tenant_id, role_id, mcp_server_id)
		SELECT $1, $3, id FROM new_servers`,
		tn.ID, n, role.ID); err != nil {
		t.Fatalf("seed %d entitlements: %v", n, err)
	}

	all, err := s.ListEntitlementsPage(ctx, tn.ID, nil, 0)
	if err != nil {
		t.Fatalf("ListEntitlementsPage(nil, 0): %v", err)
	}
	if len(all) != n {
		t.Fatalf("limit<=0 returned %d entitlements, want all %d — the unbounded path must not inherit a page cap", len(all), n)
	}
}

// TestConcurrentInsertDuringPagination documents (rather than assumes) what a
// mid-pagination insert does: a row sorting AFTER the current cursor appears on
// a later page; a row sorting BEFORE it is missed. That is inherent to keyset
// pagination and is the contract we ship.
func TestConcurrentInsertDuringPagination(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	for _, n := range []string{"srv-a", "srv-c"} {
		if _, err := s.CreateMCPServer(ctx, MCPServer{
			TenantID: tn.ID, Name: n, Transport: "http",
			EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
		}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}

	first, err := s.ListMCPServersPage(ctx, tn.ID, nil, 1)
	if err != nil || len(first) != 1 || first[0].Name != "srv-a" {
		t.Fatalf("first page = %+v, err=%v; want [srv-a]", first, err)
	}

	// Insert on BOTH sides of the cursor while pagination is in flight.
	for _, n := range []string{"srv-0", "srv-b"} { // "srv-0" < "srv-a" < "srv-b"
		if _, err := s.CreateMCPServer(ctx, MCPServer{
			TenantID: tn.ID, Name: n, Transport: "http",
			EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
		}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}

	c := MCPServerCursor(first[0])
	rest, err := s.ListMCPServersPage(ctx, tn.ID, &c, 100)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	var names []string
	for _, m := range rest {
		names = append(names, m.Name)
	}
	// srv-b sorts after the cursor → visible. srv-0 sorts before it → missed.
	if len(names) != 2 || names[0] != "srv-b" || names[1] != "srv-c" {
		t.Fatalf("second page = %v, want [srv-b srv-c]: rows inserted after the cursor are visible, rows inserted before it are not", names)
	}
}

// TestRolePageWalk pages the roles list two at a time over four roles and
// asserts the pages concatenate to the full set, in name order.
//
// role.name is UNIQUE(tenant_id, name), so unlike the entitlement walk above
// this test CANNOT prove the id tiebreaker matters: it passes unchanged with
// idKey removed from keysetTail. It also does not gate the limit — it passes
// with ListRolesPage ignoring it. What it does prove: strictly-after cursor
// semantics, the sort key and direction, RoleCursor's arity matching roleKeys,
// and tenant scoping.
func TestRolePageWalk(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	names := []string{"role-a", "role-b", "role-c", "role-d"}
	for _, n := range names {
		if _, err := s.CreateRole(ctx, tn.ID, n); err != nil {
			t.Fatalf("create role %s: %v", n, err)
		}
	}

	var got []string
	var cursor *ListCursor
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("pagination did not terminate after 20 pages of limit=2 over 4 roles")
		}
		page, err := s.ListRolesPage(ctx, tn.ID, cursor, 2)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			got = append(got, r.Name)
		}
		c := RoleCursor(page[len(page)-1])
		cursor = &c
	}

	if len(got) != len(names) {
		t.Fatalf("walked %d roles (%v), want %d", len(got), got, len(names))
	}
	for i, n := range names {
		if got[i] != n {
			t.Errorf("position %d = %q, want %q — pages must concatenate in sort order", i, got[i], n)
		}
	}
}

// TestArtifactPageStateFilterAcrossBoundary is spec §4.1's trap: the state
// filter used to run in a Go loop AFTER loading every row, so a SQL LIMIT
// beneath it would return short or empty pages while more matches existed. The
// filter must be in SQL, so a limit=2 page of a table whose matching rows are
// interleaved with non-matching ones still returns 2 matches.
func TestArtifactPageStateFilterAcrossBoundary(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	// Interleave draft/pending by name so a naive "load all, filter in Go, then
	// LIMIT" would put non-matching rows inside the first page window.
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		a, err := s.CreateArtifact(ctx, Artifact{
			TenantID: tn.ID, Type: "skill", Name: "state-" + n,
			Content: "---\nname: x\ndescription: y\n---\nbody\n",
		})
		if err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
		// Every other artifact goes pending.
		if n == "a2" || n == "a4" || n == "a6" {
			if _, err := s.SetArtifactSubmitted(ctx, tn.ID, a.ID, "submitter@example.com", []byte("[]")); err != nil {
				t.Fatalf("submit %s: %v", n, err)
			}
		}
	}

	page, err := s.ListArtifactsPage(ctx, tn.ID, ArtifactPageOpts{State: "pending", Limit: 2})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("first pending page returned %d rows, want a FULL page of 2 — the state filter must be in SQL, not applied after LIMIT", len(page))
	}
	for _, a := range page {
		if a.ApprovalState != "pending" {
			t.Errorf("artifact %s has state %q, want pending", a.Name, a.ApprovalState)
		}
	}

	c := ArtifactCursor(page[len(page)-1])
	rest, err := s.ListArtifactsPage(ctx, tn.ID, ArtifactPageOpts{State: "pending", Cursor: &c, Limit: 100})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(rest) != 1 {
		t.Fatalf("second pending page returned %d rows, want 1 (3 pending total)", len(rest))
	}
}

// TestArtifactPageSlimOmitsHeavyFieldsButKeepsApprovedFlag pins correction C2:
// the slim projection must drop the ~144 KiB payload columns WITHOUT losing the
// "a live approved snapshot exists" signal the portal renders as a badge.
func TestArtifactPageSlimOmitsHeavyFieldsButKeepsApprovedFlag(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	a, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "slim-check",
		Content:     "---\nname: slim-check\ndescription: d\n---\nBODY-SENTINEL\n",
		MemoryScope: "user", MemorySeed: "SEED-SENTINEL",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SetArtifactSubmitted(ctx, tn.ID, a.ID, "submitter@example.com", []byte("[]")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, _, err := s.SetArtifactApproved(ctx, tn.ID, a.ID, "approver@example.com", 0); err != nil {
		t.Fatalf("approve: %v", err)
	}

	slim, err := s.ListArtifactsPage(ctx, tn.ID, ArtifactPageOpts{Limit: 100})
	if err != nil || len(slim) != 1 {
		t.Fatalf("slim list = %d rows, err=%v; want 1", len(slim), err)
	}
	got := slim[0]
	if got.Content != "" || got.MemorySeed != "" || got.ApprovedContent != "" || got.ApprovedMemorySeed != "" {
		t.Errorf("slim row still carries payload: content=%q seed=%q approvedContent=%q approvedSeed=%q",
			got.Content, got.MemorySeed, got.ApprovedContent, got.ApprovedMemorySeed)
	}
	if !got.HasApproved {
		t.Error("slim row reports HasApproved=false for an approved artifact — the Live badge would silently disappear (correction C2)")
	}
	if got.MemoryScope != "user" || got.ApprovalState != "approved" || got.Name != "slim-check" {
		t.Errorf("slim row lost a light field: %+v", got)
	}

	// Exercise ArtifactPageOpts{IncludeContent: true} directly — the door
	// handleListArtifacts (Task 8) actually calls for ?include=content.
	// ListArtifactsByTenant, the unpaginated wrapper this comment used to
	// point at, is now production-dead: its sole caller (handleListArtifacts)
	// was moved onto ListArtifactsPage directly in Task 8, so it was deleted
	// rather than kept as a documented test-only alias — the same criterion
	// abf7e55/16dcf9d already applied to its four ListXByTenant siblings.
	full, err := s.ListArtifactsPage(ctx, tn.ID, ArtifactPageOpts{Limit: 100, IncludeContent: true})
	if err != nil || len(full) != 1 {
		t.Fatalf("full list = %d rows, err=%v; want 1", len(full), err)
	}
	if !strings.Contains(full[0].Content, "BODY-SENTINEL") {
		t.Errorf("IncludeContent row is missing content: %q", full[0].Content)
	}
	if full[0].MemorySeed != "SEED-SENTINEL" {
		t.Errorf("IncludeContent row is missing memorySeed: %q", full[0].MemorySeed)
	}
	if !strings.Contains(full[0].ApprovedContent, "BODY-SENTINEL") {
		t.Errorf("IncludeContent row is missing approvedContent: %q", full[0].ApprovedContent)
	}
	if !full[0].HasApproved {
		t.Error("full row reports HasApproved=false for an approved artifact")
	}
}

// TestArtifactEntitlementPageNonUniqueSortKey is the §4.2 proof for the SECOND
// non-unique-key list: artifact_entitlement also sorts on role_id, which is
// NOT unique (one role has many artifact entitlements).
//
// RED-PROOF: drop id from ListCursor/keysetTail and this test fails with
// duplicates and missing ids. If it passes with an id-less cursor, it is not
// evidence of anything — fix the test, not the code.
//
// What this does NOT prove: the unbounded (limit<=0) full-set contract (see
// TestListArtifactEntitlementsPageUnboundedReturnsEverything for that), and —
// because every page here is limit=1 — nothing about a multi-row page window.
func TestArtifactEntitlementPageNonUniqueSortKey(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "ae-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	want := map[string]bool{}
	for _, n := range []string{"ae1", "ae2", "ae3", "ae4"} {
		a, err := s.CreateArtifact(ctx, Artifact{
			TenantID: tn.ID, Type: "skill", Name: "ae-" + n, Visibility: "role",
			Content: "---\nname: x\ndescription: y\n---\nbody\n",
		})
		if err != nil {
			t.Fatalf("create artifact %s: %v", n, err)
		}
		e, err := s.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
			TenantID: tn.ID, RoleID: role.ID, ArtifactID: a.ID,
		})
		if err != nil {
			t.Fatalf("create entitlement %s: %v", n, err)
		}
		want[e.ID] = true
	}

	seen := map[string]int{}
	var cursor *ListCursor
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("pagination did not terminate after 20 pages of limit=1 over 4 rows")
		}
		page, err := s.ListArtifactEntitlementsPage(ctx, tn.ID, cursor, 1)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			seen[e.ID]++
		}
		c := ArtifactEntitlementCursor(page[len(page)-1])
		cursor = &c
	}
	for id := range want {
		if seen[id] != 1 {
			t.Errorf("artifact entitlement %s seen %d times across pages, want exactly 1", id, seen[id])
		}
	}
}

// TestListArtifactEntitlementsPageUnboundedReturnsEverything is
// TestListEntitlementsPageUnboundedReturnsEverything's counterpart for
// ListArtifactEntitlementsPage — same rationale: ListArtifactEntitlementsByTenant
// was deleted (no real caller survived Task 7), so this pins the shared
// `limit<=0 → LIMIT NULL` contract directly.
//
// n is 501 — one past maxListLimit (internal/api/paging.go, 500): a cap at or
// below 500 fails this test, a cap of 502+ would not. Seeded via a single
// bulk INSERT rather than n CreateArtifact/CreateArtifactEntitlement round
// trips (cost: low tens of milliseconds).
func TestListArtifactEntitlementsPageUnboundedReturnsEverything(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "full-set-ae-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	const n = 501
	if _, err := s.db.Exec(ctx, `
		WITH new_artifacts AS (
			INSERT INTO artifact (tenant_id, type, name, content, visibility)
			SELECT $1, 'skill', 'full-set-ae-' || g, $4, 'role'
			FROM generate_series(1, $2) AS g
			RETURNING id
		)
		INSERT INTO artifact_entitlement (tenant_id, role_id, artifact_id)
		SELECT $1, $3, id FROM new_artifacts`,
		tn.ID, n, role.ID, "---\nname: x\ndescription: y\n---\nbody\n"); err != nil {
		t.Fatalf("seed %d artifact entitlements: %v", n, err)
	}

	all, err := s.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0)
	if err != nil {
		t.Fatalf("ListArtifactEntitlementsPage(nil, 0): %v", err)
	}
	if len(all) != n {
		t.Fatalf("limit<=0 returned %d artifact entitlements, want all %d — the unbounded path must not inherit a page cap", len(all), n)
	}
}

// TestListMCPServersByTenantUnboundedReturnsEverything pins
// ListMCPServersByTenant's own full-set contract through the door production
// actually calls it: catalog.go, the admin slug-collision check
// (admin_servers.go's checkServerSlugCollision), and gateway/server.go all
// call the wrapper directly, never ListMCPServersPage(..., nil, 0) — so
// asserting through the wrapper, not the option, is what the two tests above
// actually needed to also do for THIS function and didn't.
//
// Before this test, nothing gated the wrapper's own call site:
// mcpserver_test.go's only list assertion is a 1-row set, and the two tests
// above only cover the SHARED keysetTail limit<=0 path — not a cap an
// implementer bakes directly into ListMCPServersByTenant's own call to
// ListMCPServersPage (e.g. `nil, 100`), which is a different failure mode
// entirely and was red-disproven live: injecting exactly that into the
// wrapper left the whole repo suite green.
//
// n is 501 — one past maxListLimit (internal/api/paging.go, 500): a cap at or
// below 500 fails this test, a cap of 502+ would not. Seeded via a single
// bulk INSERT (cost: low tens of milliseconds).
func TestListMCPServersByTenantUnboundedReturnsEverything(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	const n = 501
	if _, err := s.db.Exec(ctx, `
		INSERT INTO mcp_server (tenant_id, name, transport, endpoint_or_command, status)
		SELECT $1, 'full-set-mcp-' || g, 'http', 'https://example.invalid/mcp', 'active'
		FROM generate_series(1, $2) AS g`,
		tn.ID, n); err != nil {
		t.Fatalf("seed %d servers: %v", n, err)
	}

	all, err := s.ListMCPServersByTenant(ctx, tn.ID)
	if err != nil {
		t.Fatalf("ListMCPServersByTenant: %v", err)
	}
	if len(all) != n {
		t.Fatalf("unpaginated list returned %d servers, want all %d — the wrapper must not inherit a page cap", len(all), n)
	}
}

// TestArtifactRevisionPageDescendingWalk, TestArtifactRevisionPageUnknownArtifact,
// and TestListArtifactRevisionsStillReturnsEverything moved to
// paging_revisions.ee_test.go: revision pagination is Enterprise-only
// (docs/specs/2026-08-19-orbeat-community-repo-generation-design.md §4).
