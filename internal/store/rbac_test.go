package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestEntitlementsByRoles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "developers")
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	other, err := s.CreateRole(ctx, tn.ID, "admins")
	if err != nil {
		t.Fatalf("CreateRole(other): %v", err)
	}

	srv, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "github", Transport: "http",
		EndpointOrCommand: "https://x", Status: "active",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}

	ent, err := s.CreateEntitlement(ctx, Entitlement{
		TenantID:     tn.ID,
		RoleID:       role.ID,
		MCPServerID:  srv.ID,
		AllowedTools: []string{"list_repos", "create_issue"},
		Permissions:  []string{"read"},
	})
	if err != nil {
		t.Fatalf("CreateEntitlement: %v", err)
	}

	// Querying by the entitled role returns it.
	got, err := s.ListEntitlementsByRoles(ctx, tn.ID, []string{role.ID})
	if err != nil {
		t.Fatalf("ListEntitlementsByRoles: %v", err)
	}
	if len(got) != 1 || got[0].ID != ent.ID {
		t.Fatalf("got %+v, want the created entitlement", got)
	}
	if len(got[0].AllowedTools) != 2 || got[0].AllowedTools[0] != "list_repos" {
		t.Fatalf("AllowedTools = %v", got[0].AllowedTools)
	}
	if len(got[0].Permissions) != 1 || got[0].Permissions[0] != "read" {
		t.Fatalf("Permissions = %v, want [read]", got[0].Permissions)
	}

	// Querying by an unrelated role returns nothing.
	none, err := s.ListEntitlementsByRoles(ctx, tn.ID, []string{other.ID})
	if err != nil {
		t.Fatalf("ListEntitlementsByRoles(other): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no entitlements for unrelated role, got %+v", none)
	}

	// Empty role set returns nothing without error.
	empty, err := s.ListEntitlementsByRoles(ctx, tn.ID, nil)
	if err != nil {
		t.Fatalf("ListEntitlementsByRoles(empty): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected none for empty roles, got %+v", empty)
	}
}

func TestAllowedToolsNilMeansAll(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)
	role, _ := s.CreateRole(ctx, tn.ID, "r")
	srv, _ := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "s", Transport: "stdio",
		EndpointOrCommand: "cmd", Status: "active",
	})

	ent, err := s.CreateEntitlement(ctx, Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID,
		AllowedTools: nil, // nil => all tools allowed
		Permissions:  []string{},
	})
	if err != nil {
		t.Fatalf("CreateEntitlement: %v", err)
	}
	got, err := s.ListEntitlementsByRoles(ctx, tn.ID, []string{role.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].AllowedTools != nil {
		t.Fatalf("expected nil AllowedTools (all), got %+v", got[0].AllowedTools)
	}
	_ = ent
}

func TestRoleAndEntitlementLookups(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	admin, _ := s.CreateRole(ctx, tn.ID, "orbeat-admin")
	user, _ := s.CreateRole(ctx, tn.ID, "orbeat-user")

	roles, err := s.ListRolesPage(ctx, tn.ID, nil, 0)
	if err != nil || len(roles) != 2 {
		t.Fatalf("ListRolesPage: %v len=%d", err, len(roles))
	}

	got, err := s.GetRolesByNames(ctx, tn.ID, []string{"orbeat-user", "missing"})
	if err != nil {
		t.Fatalf("GetRolesByNames: %v", err)
	}
	if len(got) != 1 || got[0].ID != user.ID {
		t.Fatalf("GetRolesByNames = %+v, want only orbeat-user", got)
	}
	if _, err := s.GetRolesByNames(ctx, tn.ID, nil); err != nil {
		t.Fatalf("GetRolesByNames(nil): %v", err)
	}

	srv, _ := s.CreateMCPServer(ctx, MCPServer{TenantID: tn.ID, Name: "gh", Transport: "http", EndpointOrCommand: "https://x", Status: "active"})
	ent, _ := s.CreateEntitlement(ctx, Entitlement{TenantID: tn.ID, RoleID: admin.ID, MCPServerID: srv.ID, Permissions: []string{}})

	all, err := s.ListEntitlementsPage(ctx, tn.ID, nil, 0)
	if err != nil || len(all) != 1 || all[0].ID != ent.ID {
		t.Fatalf("ListEntitlementsPage(nil, 0): %v %+v", err, all)
	}
	if err := s.DeleteEntitlement(ctx, tn.ID, ent.ID); err != nil {
		t.Fatalf("DeleteEntitlement: %v", err)
	}
	after, _ := s.ListEntitlementsPage(ctx, tn.ID, nil, 0)
	if len(after) != 0 {
		t.Fatalf("expected 0 entitlements after delete, got %d", len(after))
	}

	// Deleting the same entitlement again must return ErrNotFound.
	if err := s.DeleteEntitlement(ctx, tn.ID, ent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteEntitlement: want ErrNotFound on second delete, got %v", err)
	}
}

// TestAllowedToolsEmptyNonNilRoundTrip verifies the critical security invariant:
// an entitlement stored with AllowedTools == []string{} (empty, non-nil) must
// round-trip back as a non-nil empty slice, not as nil.
// nil means "all tools allowed"; empty-non-nil means "no tools allowed" (deny-all).
// Conflating the two is a security bug.
func TestAllowedToolsEmptyNonNilRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)
	role, _ := s.CreateRole(ctx, tn.ID, "r-empty")
	srv, _ := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "s-empty", Transport: "stdio",
		EndpointOrCommand: "cmd", Status: "active",
	})

	// Store with explicitly empty (non-nil) AllowedTools => deny-all.
	_, err := s.CreateEntitlement(ctx, Entitlement{
		TenantID:     tn.ID,
		RoleID:       role.ID,
		MCPServerID:  srv.ID,
		AllowedTools: []string{}, // non-nil, length 0
		Permissions:  []string{},
	})
	if err != nil {
		t.Fatalf("CreateEntitlement: %v", err)
	}

	got, err := s.ListEntitlementsByRoles(ctx, tn.ID, []string{role.ID})
	if err != nil {
		t.Fatalf("ListEntitlementsByRoles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entitlement, got %d", len(got))
	}

	tools := got[0].AllowedTools
	// CRITICAL: must be non-nil (deny-all), not nil (allow-all).
	if tools == nil {
		t.Fatal("AllowedTools round-tripped as nil (allow-all) instead of empty non-nil (deny-all): security invariant violated")
	}
	if len(tools) != 0 {
		t.Fatalf("AllowedTools = %v, want empty non-nil slice", tools)
	}
}

// TestRoleExistsInTenantMalformedID pins the sibling of the mcp_server fix: id
// reaches RoleExistsInTenant unvalidated from a JSON body
// (handleCreateEntitlement), so a malformed value fails Postgres' uuid cast
// rather than matching no row. That must read as "doesn't exist" — (false, nil)
// — not as an error, which the handler would surface as a 500. Both ids in that
// one request are now consistent: a malformed roleId and a malformed
// mcpServerId behave the same.
func TestRoleExistsInTenantMalformedID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	if ok, err := s.RoleExistsInTenant(ctx, tn.ID, "not-a-uuid"); err != nil || ok {
		t.Fatalf("RoleExistsInTenant(malformed id): want (false, nil), got (%v, %v)", ok, err)
	}
}

// TestDeleteEntitlementMalformedIDIsNotFound proves a non-UUID id is treated
// as ErrNotFound (mapping Postgres 22P02 invalid_text_representation), not
// surfaced as a raw driver error that would 500 at the API layer (audit B2b)
// — mirrors TestArtifactMalformedIDIsNotFound / TestMCPServerMalformedIDIsNotFound.
func TestDeleteEntitlementMalformedIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	if err := s.DeleteEntitlement(ctx, tn.ID, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteEntitlement(bad id): want ErrNotFound, got %v", err)
	}
}

// TestDeleteRoleReportsWhatTheCascadeRevoked is the decisive test for this
// slice. role is referenced ON DELETE CASCADE from entitlement and
// artifact_entitlement, so deleting it silently revokes every grant hung off
// it. DeleteRole must report exactly what was destroyed — the audit record
// built from it is the only thing that can later answer "why did alice lose
// access?".
func TestDeleteRoleReportsWhatTheCascadeRevoked(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "del-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	// Two server grants.
	for i := 0; i < 2; i++ {
		srv, err := s.CreateMCPServer(ctx, MCPServer{
			TenantID: tn.ID, Name: seqName("del-srv", i), Transport: "http",
			EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
		})
		if err != nil {
			t.Fatalf("create server %d: %v", i, err)
		}
		if _, err := s.CreateEntitlement(ctx, Entitlement{
			TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID,
		}); err != nil {
			t.Fatalf("create entitlement %d: %v", i, err)
		}
	}

	// One artifact grant.
	art, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "del-art", Visibility: "role",
		Content: "---\nname: x\ndescription: y\n---\nbody\n",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if _, err := s.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
		TenantID: tn.ID, RoleID: role.ID, ArtifactID: art.ID,
	}); err != nil {
		t.Fatalf("create artifact entitlement: %v", err)
	}

	got, err := s.DeleteRole(ctx, tn.ID, role.ID)
	if err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if got.RoleName != "del-role" {
		t.Errorf("RoleName = %q, want %q", got.RoleName, "del-role")
	}
	if got.Entitlements != 2 || got.ArtifactEntitlements != 1 {
		t.Errorf("revoked = %d entitlements / %d artifact entitlements, want 2/1",
			got.Entitlements, got.ArtifactEntitlements)
	}
	// Assert the actual name SETS, not just their length — a mutant that
	// selected m.id::text / a.id::text instead of m.name / a.name would still
	// satisfy a len()-only check.
	if !slices.Equal(got.ServerNames, []string{"del-srv-0", "del-srv-1"}) {
		t.Errorf("ServerNames = %v, want [del-srv-0 del-srv-1]", got.ServerNames)
	}
	if !slices.Equal(got.ArtifactNames, []string{"del-art"}) {
		t.Errorf("ArtifactNames = %v, want [del-art]", got.ArtifactNames)
	}

	// The cascade actually fired.
	ents, err := s.ListEntitlementsPage(ctx, tn.ID, nil, 0)
	if err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	for _, e := range ents {
		if e.RoleID == role.ID {
			t.Errorf("entitlement %s survived the role deletion", e.ID)
		}
	}
	aents, err := s.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0)
	if err != nil {
		t.Fatalf("list artifact entitlements: %v", err)
	}
	for _, e := range aents {
		if e.RoleID == role.ID {
			t.Errorf("artifact entitlement %s survived the role deletion", e.ID)
		}
	}
}

// TestDeleteRoleWithNoGrants pins the zero case — a fresh role deletes cleanly
// and reports zeroes rather than erroring or returning nil slices that a
// caller would have to special-case. "Special-case" is made concrete here:
// ServerNames/ArtifactNames must marshal to JSON `[]`, not `null` — Task 2's
// audit metadata embeds these directly, and a bare `encoding/json` marshal of
// a nil slice produces `null`, which reads as "unknown" rather than "zero" in
// an audit record.
func TestDeleteRoleWithNoGrants(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "lonely-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	got, err := s.DeleteRole(ctx, tn.ID, role.ID)
	if err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if got.RoleName != "lonely-role" {
		t.Errorf("RoleName = %q, want %q", got.RoleName, "lonely-role")
	}
	if got.Entitlements != 0 || got.ArtifactEntitlements != 0 {
		t.Errorf("revoked = %d/%d, want 0/0", got.Entitlements, got.ArtifactEntitlements)
	}
	if got.ServerNames == nil {
		t.Error("ServerNames is nil for a zero-grant role, want a non-nil empty slice")
	}
	if got.ArtifactNames == nil {
		t.Error("ArtifactNames is nil for a zero-grant role, want a non-nil empty slice")
	}
	if b, err := json.Marshal(got.ServerNames); err != nil || string(b) != "[]" {
		t.Errorf("json.Marshal(ServerNames) = %s (err=%v), want []", b, err)
	}
	if b, err := json.Marshal(got.ArtifactNames); err != nil || string(b) != "[]" {
		t.Errorf("json.Marshal(ArtifactNames) = %s (err=%v), want []", b, err)
	}
}

// TestDeleteRoleScopesGrantsToRole pins the "AND e.role_id = $2" / "AND
// ae.role_id = $2" predicates in DeleteRole's grant reads. A role with NO
// siblings would pass even with the predicate dropped (there would be nothing
// else in the tenant to leak in) — a sibling role with its OWN grants is what
// makes the predicate load-bearing: drop it, and the sibling's server/artifact
// names and counts leak into the target role's report, and the reported set
// stops matching what its own DELETE cascades.
func TestDeleteRoleScopesGrantsToRole(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	target, err := s.CreateRole(ctx, tn.ID, "scope-target")
	if err != nil {
		t.Fatalf("create target role: %v", err)
	}
	sibling, err := s.CreateRole(ctx, tn.ID, "scope-sibling")
	if err != nil {
		t.Fatalf("create sibling role: %v", err)
	}

	targetSrv, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "scope-target-srv", Transport: "http",
		EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("create target server: %v", err)
	}
	if _, err := s.CreateEntitlement(ctx, Entitlement{
		TenantID: tn.ID, RoleID: target.ID, MCPServerID: targetSrv.ID,
	}); err != nil {
		t.Fatalf("create target entitlement: %v", err)
	}

	siblingSrv, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "scope-sibling-srv", Transport: "http",
		EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("create sibling server: %v", err)
	}
	siblingEnt, err := s.CreateEntitlement(ctx, Entitlement{
		TenantID: tn.ID, RoleID: sibling.ID, MCPServerID: siblingSrv.ID,
	})
	if err != nil {
		t.Fatalf("create sibling entitlement: %v", err)
	}

	targetArt, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "scope-target-art", Visibility: "role",
		Content: "---\nname: x\ndescription: y\n---\nbody\n",
	})
	if err != nil {
		t.Fatalf("create target artifact: %v", err)
	}
	if _, err := s.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
		TenantID: tn.ID, RoleID: target.ID, ArtifactID: targetArt.ID,
	}); err != nil {
		t.Fatalf("create target artifact entitlement: %v", err)
	}

	siblingArt, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "scope-sibling-art", Visibility: "role",
		Content: "---\nname: x\ndescription: y\n---\nbody\n",
	})
	if err != nil {
		t.Fatalf("create sibling artifact: %v", err)
	}
	siblingArtEnt, err := s.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
		TenantID: tn.ID, RoleID: sibling.ID, ArtifactID: siblingArt.ID,
	})
	if err != nil {
		t.Fatalf("create sibling artifact entitlement: %v", err)
	}

	got, err := s.DeleteRole(ctx, tn.ID, target.ID)
	if err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if got.RoleName != "scope-target" {
		t.Errorf("RoleName = %q, want %q (a sibling role's own name leaked)", got.RoleName, "scope-target")
	}
	if got.Entitlements != 1 || got.ArtifactEntitlements != 1 {
		t.Fatalf("revoked = %d/%d, want 1/1 (a sibling role's own grants leaked into the count)",
			got.Entitlements, got.ArtifactEntitlements)
	}
	if !slices.Equal(got.ServerNames, []string{"scope-target-srv"}) {
		t.Errorf("ServerNames = %v, want [scope-target-srv]", got.ServerNames)
	}
	if !slices.Equal(got.ArtifactNames, []string{"scope-target-art"}) {
		t.Errorf("ArtifactNames = %v, want [scope-target-art]", got.ArtifactNames)
	}

	// The sibling role's own grants must survive untouched.
	ents, err := s.ListEntitlementsPage(ctx, tn.ID, nil, 0)
	if err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	found := false
	for _, e := range ents {
		if e.ID == siblingEnt.ID {
			found = true
		}
	}
	if !found {
		t.Error("sibling role's entitlement is gone after deleting an unrelated role")
	}

	aents, err := s.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0)
	if err != nil {
		t.Fatalf("list artifact entitlements: %v", err)
	}
	found = false
	for _, e := range aents {
		if e.ID == siblingArtEnt.ID {
			found = true
		}
	}
	if !found {
		t.Error("sibling role's artifact entitlement is gone after deleting an unrelated role")
	}
}

// TestDeleteRoleCapsNameListsAndReportsTruncated pins maxGrantNames: the
// name lists reported by a role deletion must never grow without bound (a
// role granted thousands of servers must not write an unbounded jsonb blob
// into audit_event), while the count stays exact regardless of truncation.
func TestDeleteRoleCapsNameListsAndReportsTruncated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "cap-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	const total = maxGrantNames + 1
	for i := 0; i < total; i++ {
		srv, err := s.CreateMCPServer(ctx, MCPServer{
			TenantID: tn.ID, Name: seqName("cap-srv", i), Transport: "http",
			EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
		})
		if err != nil {
			t.Fatalf("create server %d: %v", i, err)
		}
		if _, err := s.CreateEntitlement(ctx, Entitlement{
			TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID,
		}); err != nil {
			t.Fatalf("create entitlement %d: %v", i, err)
		}
	}

	got, err := s.DeleteRole(ctx, tn.ID, role.ID)
	if err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if got.Entitlements != total {
		t.Errorf("Entitlements = %d, want %d (the count must stay exact even when the list truncates)",
			got.Entitlements, total)
	}
	if len(got.ServerNames) != maxGrantNames {
		t.Errorf("len(ServerNames) = %d, want %d (the cap)", len(got.ServerNames), maxGrantNames)
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true")
	}
}

// waitForBlockedQuery polls pg_stat_activity until it finds a backend whose
// current query text contains substr and is waiting on a lock, or fails the
// test after timeout. Used by the race test below instead of a fixed sleep,
// so the interleaving it depends on does not flake under CI scheduling
// jitter — and so the test cannot silently "pass" because the racing
// statement finished before the intended interleaving ever happened.
func waitForBlockedQuery(t *testing.T, s *Store, substr string, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		err := s.db.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query ILIKE '%' || $1 || '%'`, substr).Scan(&n)
		if err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for a blocked query matching %q", timeout, substr)
}

// TestDeleteRoleLocksAgainstConcurrentGrantInsert reproduces a race found in
// review: RoleExistsInTenant takes a FOR SHARE lock on the role row and is
// held open by handleCreateEntitlement / handleCreateArtifactEntitlement
// across their own INSERT. Under this repo's READ COMMITTED isolation, if
// DeleteRole reads its "before" picture WITHOUT first taking a conflicting
// lock, those reads can complete, then DeleteRole blocks on its final DELETE
// (which needs an exclusive lock the FOR SHARE holder is blocking) — and
// while it waits, the holder can insert ANOTHER grant and commit, releasing
// the lock. DeleteRole's DELETE then proceeds and cascades N+1 rows while its
// report says N: the audit record under-reports what was actually revoked.
//
// This test models that interleaving directly with two real, concurrent
// transactions (not a mock): a "holder" tx takes RoleExistsInTenant's lock
// and keeps it open; DeleteRole runs concurrently, in a real caller-owned
// transaction (store.InTx, matching how Task 2's handler calls it); once
// DeleteRole is observed blocked on a lock, the holder inserts one more grant
// and commits. DeleteRole must then report ALL THREE grants, not just the two
// that existed when it started reading.
func TestDeleteRoleLocksAgainstConcurrentGrantInsert(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "race-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	// Two pre-existing server grants — what DeleteRole would see if it read
	// right now.
	for i := 0; i < 2; i++ {
		srv, err := s.CreateMCPServer(ctx, MCPServer{
			TenantID: tn.ID, Name: seqName("race-srv", i), Transport: "http",
			EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
		})
		if err != nil {
			t.Fatalf("create server %d: %v", i, err)
		}
		if _, err := s.CreateEntitlement(ctx, Entitlement{
			TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID,
		}); err != nil {
			t.Fatalf("create entitlement %d: %v", i, err)
		}
	}

	// The third server, granted mid-race by the concurrent holder tx.
	raceSrv, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "race-srv-inserted", Transport: "http",
		EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("create race server: %v", err)
	}

	// The holder: a real, separate, still-open transaction that takes
	// RoleExistsInTenant's FOR SHARE lock — modelling
	// handleCreateEntitlement's existence check before its own INSERT.
	pgtx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer pgtx.Rollback(ctx) // no-op once committed below
	holder := &Store{db: pgtx}
	if ok, err := holder.RoleExistsInTenant(ctx, tn.ID, role.ID); err != nil || !ok {
		t.Fatalf("RoleExistsInTenant: ok=%v err=%v", ok, err)
	}

	// Run DeleteRole concurrently, inside InTx — a bare pool call would be
	// its own separate implicit transaction and could never contend for the
	// lock the way Task 2's auditedTx-wrapped call does.
	type result struct {
		g   RevokedGrants
		err error
	}
	done := make(chan result, 1)
	go func() {
		var g RevokedGrants
		err := s.InTx(ctx, func(tx *Store) error {
			var err error
			g, err = tx.DeleteRole(ctx, tn.ID, role.ID)
			return err
		})
		done <- result{g, err}
	}()

	// Wait until DeleteRole's transaction is genuinely blocked on a lock
	// before the holder inserts and commits.
	waitForBlockedQuery(t, s, "role", 5*time.Second)

	if _, err := holder.CreateEntitlement(ctx, Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: raceSrv.ID,
	}); err != nil {
		t.Fatalf("holder insert entitlement: %v", err)
	}
	if err := pgtx.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("DeleteRole: %v", res.err)
		}
		if res.g.Entitlements != 3 {
			t.Errorf("DeleteRole reported %d entitlements revoked, want 3 (the grant inserted while DeleteRole was blocked was not counted)",
				res.g.Entitlements)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteRole did not return after the holder committed")
	}
}

// TestDeleteRoleInsideInTx exercises DeleteRole the way Task 2's handler
// actually calls it: wrapped in a single caller-owned transaction
// (store.InTx / auditedTx), not pool-backed. Every other DeleteRole test in
// this file uses the pool-backed *Store from newTestStore, so each call runs
// in its own separate implicit transaction — giving the "runs in the
// caller's transaction" guarantee (this file's DeleteRole doc comment) zero
// direct coverage, and the row lock above zero chance to matter (it can only
// block a concurrent holder for as long as the CALLER's transaction stays
// open).
func TestDeleteRoleInsideInTx(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "intx-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	srv, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "intx-srv", Transport: "http",
		EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if _, err := s.CreateEntitlement(ctx, Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID,
	}); err != nil {
		t.Fatalf("create entitlement: %v", err)
	}

	var got RevokedGrants
	err = s.InTx(ctx, func(tx *Store) error {
		var err error
		got, err = tx.DeleteRole(ctx, tn.ID, role.ID)
		return err
	})
	if err != nil {
		t.Fatalf("DeleteRole inside InTx: %v", err)
	}
	if got.Entitlements != 1 {
		t.Errorf("Entitlements = %d, want 1", got.Entitlements)
	}

	// The role and its grant are actually gone once the outer tx has committed.
	roles, err := s.ListRolesPage(ctx, tn.ID, nil, 0)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	for _, r := range roles {
		if r.ID == role.ID {
			t.Errorf("role %s survived DeleteRole run inside InTx", r.ID)
		}
	}
}

// TestDeleteRoleNotFound covers all three shapes that must be 404 at the API
// layer. The malformed-id case is NOT optional: v1.16.0 established
// malformed-id-must-404 via idCastNotFound, and that mapping already regressed
// once during the optimistic-concurrency slice.
func TestDeleteRoleNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	other := mustTenant(t, s)

	foreign, err := s.CreateRole(ctx, other.ID, "foreign-role")
	if err != nil {
		t.Fatalf("create foreign role: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"unknown uuid", "00000000-0000-0000-0000-000000000000"},
		{"malformed id", "not-a-uuid"},
		{"cross-tenant id", foreign.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.DeleteRole(ctx, tn.ID, tc.id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("DeleteRole(%s): want ErrNotFound, got %v", tc.name, err)
			}
		})
	}

	// The foreign role must still exist.
	if _, err := s.DeleteRole(ctx, other.ID, foreign.ID); err != nil {
		t.Fatalf("foreign role should still be deletable by its owner: %v", err)
	}
}
