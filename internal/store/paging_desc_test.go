package store

import (
	"context"
	"testing"
)

// This file's five tests are docs/plans/orbeat-admin-search-sort-2026-08-27.md
// Task 3's "multi-page walk under a non-default sort returns every row
// exactly once" requirement, for the one new axis that task adds: ?order.
// Every list here has exactly one allowlisted ?sort column (Task 2's
// decision, see internal/api/paging.go's allowlist comment), so the only
// walkable non-default order is desc=true. "Page two is non-empty" is
// explicitly NOT sufficient (this repo has shipped that exact insufficient
// assertion before): each test below collects every id seen across the
// FULL walk and compares it to the complete expected set, catching a skip
// or a duplicate that a shallower check would miss. The sixth list,
// virtual_key, is Enterprise-only and its descending walk test lives in
// virtual_key.ee_test.go instead (TestListVirtualKeysPageWalkDescending).

// TestRolePageWalkDescending mirrors TestRolePageWalk (paging_test.go) under
// desc=true: role.name is UNIQUE(tenant_id, name), so this cannot prove the
// id tiebreaker matters, but it does prove the DIRECTION is actually
// reversed end to end, RolePageSQL's ORDER BY, RoleCursor's Sort identity,
// and keysetTail's comparison operator all have to agree, or this fails
// either with a wrong order or an ErrCursorSortMismatch on page 2.
func TestRolePageWalkDescending(t *testing.T) {
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
		page, err := s.ListRolesPage(ctx, tn.ID, cursor, 2, true, "")
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			got = append(got, r.Name)
		}
		c := RoleCursor(page[len(page)-1], true)
		cursor = &c
	}

	want := []string{"role-d", "role-c", "role-b", "role-a"}
	if len(got) != len(want) {
		t.Fatalf("walked %d roles (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i, n := range want {
		if got[i] != n {
			t.Errorf("position %d = %q, want %q, desc=true must reverse the full order, not just the first page", i, got[i], n)
		}
	}
}

// TestMCPServerPageWalkDescending mirrors TestRolePageWalkDescending for
// mcp_server: same UNIQUE(tenant_id, name) shape, same reason it cannot prove
// the id tiebreaker, same reason it DOES prove direction end to end.
func TestMCPServerPageWalkDescending(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	names := []string{"srv-a", "srv-b", "srv-c", "srv-d"}
	for _, n := range names {
		if _, err := s.CreateMCPServer(ctx, MCPServer{
			TenantID: tn.ID, Name: n, Transport: "http",
			EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
		}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}

	var got []string
	var cursor *ListCursor
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("pagination did not terminate after 20 pages of limit=2 over 4 servers")
		}
		page, err := s.ListMCPServersPage(ctx, tn.ID, cursor, 2, true, "")
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			got = append(got, m.Name)
		}
		c := MCPServerCursor(page[len(page)-1], true)
		cursor = &c
	}

	want := []string{"srv-d", "srv-c", "srv-b", "srv-a"}
	if len(got) != len(want) {
		t.Fatalf("walked %d servers (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i, n := range want {
		if got[i] != n {
			t.Errorf("position %d = %q, want %q", i, got[i], n)
		}
	}
}

// TestEntitlementPageNonUniqueSortKeyDescending mirrors
// TestEntitlementPageNonUniqueSortKey (paging_test.go) under desc=true.
// entitlement's sort key, role_id, is NOT unique, this is the list where a
// broken id tiebreaker (or a broken direction wired only halfway through the
// stack) shows up as an actual skip or duplicate, not just a wrong order.
func TestEntitlementPageNonUniqueSortKeyDescending(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "paging-role-desc")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		srv, err := s.CreateMCPServer(ctx, MCPServer{
			TenantID: tn.ID, Name: seqName("paging-srv-desc", i), Transport: "http",
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
		page, err := s.ListEntitlementsPage(ctx, tn.ID, cursor, 1, true)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			seen[e.ID]++
		}
		c := EntitlementCursor(page[len(page)-1], true)
		cursor = &c
	}

	for id := range want {
		switch seen[id] {
		case 1: // exactly once, correct
		case 0:
			t.Errorf("entitlement %s was SKIPPED across a page boundary (desc walk)", id)
		default:
			t.Errorf("entitlement %s was returned %d times (duplicated across pages, desc walk)", id, seen[id])
		}
	}
	for id := range seen {
		if !want[id] {
			t.Errorf("unexpected entitlement %s in results", id)
		}
	}
}

// TestArtifactEntitlementPageNonUniqueSortKeyDescending mirrors
// TestArtifactEntitlementPageNonUniqueSortKey under desc=true, the same
// non-unique-role_id shape as entitlement, above.
func TestArtifactEntitlementPageNonUniqueSortKeyDescending(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "paging-ae-role-desc")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		a, err := s.CreateArtifact(ctx, Artifact{
			TenantID: tn.ID, Type: "skill", Name: seqName("paging-ae-art-desc", i),
			Content: "---\nname: x\ndescription: y\n---\nbody\n",
		})
		if err != nil {
			t.Fatalf("create artifact %d: %v", i, err)
		}
		e, err := s.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
			TenantID: tn.ID, RoleID: role.ID, ArtifactID: a.ID,
		})
		if err != nil {
			t.Fatalf("create artifact entitlement %d: %v", i, err)
		}
		want[e.ID] = true
	}

	seen := map[string]int{}
	var cursor *ListCursor
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("pagination did not terminate after 20 pages of limit=1 over 5 rows")
		}
		page, err := s.ListArtifactEntitlementsPage(ctx, tn.ID, cursor, 1, true)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			seen[e.ID]++
		}
		c := ArtifactEntitlementCursor(page[len(page)-1], true)
		cursor = &c
	}

	for id := range want {
		switch seen[id] {
		case 1: // exactly once, correct
		case 0:
			t.Errorf("artifact entitlement %s was SKIPPED across a page boundary (desc walk)", id)
		default:
			t.Errorf("artifact entitlement %s was returned %d times (duplicated across pages, desc walk)", id, seen[id])
		}
	}
	for id := range seen {
		if !want[id] {
			t.Errorf("unexpected artifact entitlement %s in results", id)
		}
	}
}

// TestArtifactPageWalkDescending is artifact's version of this file's other
// four tests: two sort keys (type, name) rather than one, so it also proves
// desc=true reverses BOTH keys AND id uniformly (keysetTail's own contract --
// "a Postgres row comparison cannot express mixed directions"), not just the
// leading one.
func TestArtifactPageWalkDescending(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	type seed struct{ typ, name string }
	seeds := []seed{
		{"rule", "art-a"},
		{"skill", "art-b"},
		{"skill", "art-a"},
		{"subagent", "art-a"},
	}
	want := map[string]bool{}
	for _, sd := range seeds {
		a, err := s.CreateArtifact(ctx, Artifact{
			TenantID: tn.ID, Type: sd.typ, Name: sd.name,
			Content: "---\nname: x\ndescription: y\n---\nbody\n",
		})
		if err != nil {
			t.Fatalf("create %s/%s: %v", sd.typ, sd.name, err)
		}
		want[a.ID] = true
	}

	// Expected order descending: (type DESC, name DESC), "subagent" >
	// "skill" > "rule" lexically, and within "skill", "art-b" > "art-a".
	wantOrder := []string{"subagent/art-a", "skill/art-b", "skill/art-a", "rule/art-a"}

	seen := map[string]int{}
	var gotOrder []string
	var cursor *ListCursor
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("pagination did not terminate after 20 pages of limit=1 over 4 artifacts")
		}
		page, err := s.ListArtifactsPage(ctx, tn.ID, ArtifactPageOpts{Cursor: cursor, Limit: 1, Desc: true})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			seen[a.ID]++
			gotOrder = append(gotOrder, a.Type+"/"+a.Name)
		}
		c := ArtifactCursor(page[len(page)-1], true)
		cursor = &c
	}

	for id := range want {
		if seen[id] != 1 {
			t.Errorf("artifact %s seen %d times across the desc walk, want exactly 1", id, seen[id])
		}
	}
	for id := range seen {
		if !want[id] {
			t.Errorf("unexpected artifact %s in results", id)
		}
	}
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("walked %d artifacts (%v), want %d (%v)", len(gotOrder), gotOrder, len(wantOrder), wantOrder)
	}
	for i, w := range wantOrder {
		if gotOrder[i] != w {
			t.Errorf("position %d = %q, want %q, desc=true must reverse (type, name, id) uniformly", i, gotOrder[i], w)
		}
	}
}
