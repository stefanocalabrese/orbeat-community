package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

	roles, err := s.ListRolesPage(ctx, tn.ID, nil, 0, false, "")
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

	all, err := s.ListEntitlementsPage(ctx, tn.ID, nil, 0, false)
	if err != nil || len(all) != 1 || all[0].ID != ent.ID {
		t.Fatalf("ListEntitlementsPage(nil, 0): %v %+v", err, all)
	}
	if err := s.DeleteEntitlement(ctx, tn.ID, ent.ID); err != nil {
		t.Fatalf("DeleteEntitlement: %v", err)
	}
	after, _ := s.ListEntitlementsPage(ctx, tn.ID, nil, 0, false)
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
	ents, err := s.ListEntitlementsPage(ctx, tn.ID, nil, 0, false)
	if err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	for _, e := range ents {
		if e.RoleID == role.ID {
			t.Errorf("entitlement %s survived the role deletion", e.ID)
		}
	}
	aents, err := s.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0, false)
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
	ents, err := s.ListEntitlementsPage(ctx, tn.ID, nil, 0, false)
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

	aents, err := s.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0, false)
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

// TestDeleteRoleCapsNameListsAndReportsTruncated pins MaxGrantNames on the
// SERVER list: a role granted thousands of servers must not write an
// unbounded jsonb blob into audit_event, and the count stays exact
// regardless of truncation.
//
// It says nothing about the other two lists, and since 2026-08-30 that is
// worth stating rather than leaving to be inferred. The artifact list is
// capped the same way and has no gate of its own: MEASURED, not inferred,
// by neutralising the artTrunc term of DeleteRole's Truncated expression
// (`srvTrunc && (artTrunc || true)`, which is srvTrunc alone) and watching
// internal/store AND internal/api both stay green. Left open deliberately
// rather than fixed in passing; removing the keyTrunc term is what made it
// visible, and it was equally ungated before. The virtual-key list is
// deliberately NOT capped any more, and
// TestDeleteRoleVirtualKeyListIsNeverCapped below is what holds that.
func TestDeleteRoleCapsNameListsAndReportsTruncated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "cap-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	const total = MaxGrantNames + 1
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
	if len(got.ServerNames) != MaxGrantNames {
		t.Errorf("len(ServerNames) = %d, want %d (the cap)", len(got.ServerNames), MaxGrantNames)
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
	roles, err := s.ListRolesPage(ctx, tn.ID, nil, 0, false, "")
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

// TestUpdateRoleNameHappyPath renames a role, bumping row_version and
// returning the fresh row via the same re-read-after-write pattern as
// UpdateArtifact/UpdateMCPServer/UpdateEntitlement (each returns
// s.Get<Thing> after a successful CTE update rather than trusting a RETURNING
// clause the CTE shape does not have room for).
func TestUpdateRoleNameHappyPath(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	r, err := s.CreateRole(ctx, tn.ID, "rename-before")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	got, err := s.UpdateRoleName(ctx, tn.ID, r.ID, "rename-after", r.RowVersion)
	if err != nil {
		t.Fatalf("UpdateRoleName: %v", err)
	}
	if got.Name != "rename-after" {
		t.Fatalf("Name = %q, want rename-after", got.Name)
	}
	if got.RowVersion <= r.RowVersion {
		t.Fatalf("RowVersion = %d, want > %d", got.RowVersion, r.RowVersion)
	}
	if got.ID != r.ID || got.TenantID != tn.ID {
		t.Fatalf("got %+v, identity fields must be unchanged", got)
	}
}

// TestUpdateRoleNameRejectsStaleVersion is the decisive lost-update case,
// mirroring TestUpdateArtifactRejectsStaleVersion/TestUpdateMCPServerRejects-
// StaleVersion (concurrency_test.go). It adds one rigor beyond those two: the
// read-back after the rejected write goes through a SEPARATE Store (a second
// newTestStore(t), a genuinely different pgxpool), not the writer's own
// handle — so the assertion proves what actually committed to Postgres, not
// something a single connection's read-your-own-writes view could show even
// if the rejected UPDATE had secretly landed. Whole-struct comparison, not one
// field, so a mutation the field-by-field version of this test would miss (a
// changed TenantID, say) cannot slip through either.
func TestUpdateRoleNameRejectsStaleVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	r, err := s.CreateRole(ctx, tn.ID, "stale-before")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	staleVersion := r.RowVersion // the version an earlier "reader" would have seen

	// A first, legitimate rename using the version it read.
	first, err := s.UpdateRoleName(ctx, tn.ID, r.ID, "stale-first", staleVersion)
	if err != nil {
		t.Fatalf("first rename: %v", err)
	}

	// A second writer retries with the version it read BEFORE the first
	// rename landed (e.g. a stale form, or a naive retry after a timeout).
	if _, err := s.UpdateRoleName(ctx, tn.ID, r.ID, "stale-SHOULD-NOT-LAND", staleVersion); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("stale UpdateRoleName: want ErrVersionMismatch, got %v", err)
	}

	fresh := newTestStore(t)
	got, err := fresh.GetRole(ctx, tn.ID, r.ID)
	if err != nil {
		t.Fatalf("fresh get: %v", err)
	}
	want := Role{ID: r.ID, TenantID: tn.ID, Name: "stale-first", RowVersion: first.RowVersion}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the stale write mutated something: got %+v, want %+v — a rejected "+
			"UpdateRoleName must never touch the row", got, want)
	}
}

// TestUpdateRoleNameCollision proves the UNIQUE (tenant_id, name) constraint
// from 00001_init.sql surfaces as ErrNameTaken — a named error the API layer
// can map to 409 without inspecting a raw pgconn.PgError itself, mirroring
// artifact.go's ApprovedIdentityConflict discipline (a 23505 turned into
// something the caller can errors.Is against) — and that the identical name
// in a DIFFERENT tenant is unaffected, proving the constraint is
// tenant-scoped rather than global.
func TestUpdateRoleNameCollision(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	other := mustTenant(t, s)

	if _, err := s.CreateRole(ctx, tn.ID, "collision-taken"); err != nil {
		t.Fatalf("create taken role: %v", err)
	}
	mover, err := s.CreateRole(ctx, tn.ID, "collision-mover")
	if err != nil {
		t.Fatalf("create mover role: %v", err)
	}

	if _, err := s.UpdateRoleName(ctx, tn.ID, mover.ID, "collision-taken", mover.RowVersion); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same-tenant collision: want ErrNameTaken, got %v", err)
	}

	// mover must be untouched by the refused write.
	stillMover, err := s.GetRole(ctx, tn.ID, mover.ID)
	if err != nil {
		t.Fatalf("get mover: %v", err)
	}
	if stillMover.Name != "collision-mover" || stillMover.RowVersion != mover.RowVersion {
		t.Fatalf("a refused collision mutated mover: %+v", stillMover)
	}

	// The identical name string in a DIFFERENT tenant is a different
	// namespace entirely — this must succeed.
	crossMover, err := s.CreateRole(ctx, other.ID, "cross-mover")
	if err != nil {
		t.Fatalf("create cross-tenant mover: %v", err)
	}
	renamed, err := s.UpdateRoleName(ctx, other.ID, crossMover.ID, "collision-taken", crossMover.RowVersion)
	if err != nil {
		t.Fatalf("cross-tenant rename onto the same name string: want success, got %v", err)
	}
	if renamed.Name != "collision-taken" {
		t.Fatalf("Name = %q, want collision-taken", renamed.Name)
	}
}

// TestUpdateRoleNameNotFound mirrors TestDeleteRoleNotFound's three shapes
// exactly (same three cases, same reasoning): an unknown uuid, a malformed
// id (v1.16.0's malformed-id-must-404 class, via idCastNotFound), and a
// cross-tenant id all read as "doesn't exist for this tenant" and must never
// surface as a 500.
func TestUpdateRoleNameNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	other := mustTenant(t, s)

	foreign, err := s.CreateRole(ctx, other.ID, "foreign-role-rename")
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
			if _, err := s.UpdateRoleName(ctx, tn.ID, tc.id, "whatever", 1); !errors.Is(err, ErrNotFound) {
				t.Fatalf("UpdateRoleName(%s): want ErrNotFound, got %v", tc.name, err)
			}
		})
	}

	// The cross-tenant attempt above must not have mutated the foreign role
	// AT ALL, in the DB, not just in the ErrNotFound it returned. This is the
	// v1.16.0 defense-in-depth requirement (tenant-scoped in SQL, not only in
	// Go): the UPDATE and its existence check are two separate CTEs in the
	// SAME statement, and Postgres evaluates every CTE in a WITH clause
	// regardless of which one the outer SELECT ends up reading — so a WHERE
	// clause missing tenant_id on the UPDATE CTE would let a wrong-tenant
	// caller silently rename another tenant's role even while the Go code,
	// via the separately-scoped "cur" CTE, still (correctly, but now
	// incompletely) reports ErrNotFound. Read through the OWNING tenant so
	// this cannot pass by accident of scoping the read the same wrong way the
	// write was scoped.
	stillForeign, err := s.GetRole(ctx, other.ID, foreign.ID)
	if err != nil {
		t.Fatalf("get foreign role: %v", err)
	}
	if stillForeign.Name != "foreign-role-rename" || stillForeign.RowVersion != foreign.RowVersion {
		t.Fatalf("a cross-tenant UpdateRoleName call mutated the foreign role despite reporting "+
			"ErrNotFound: got %+v, want name=foreign-role-rename row_version=%d unchanged",
			stillForeign, foreign.RowVersion)
	}

	// The foreign role must be renamable by its own tenant.
	if _, err := s.UpdateRoleName(ctx, other.ID, foreign.ID, "foreign-role-renamed", foreign.RowVersion); err != nil {
		t.Fatalf("foreign role should still be renamable by its owner: %v", err)
	}
}

// TestUpdateRoleNameToOwnCurrentNameIsNotAnError mirrors TestNoOpUpdateStill-
// BumpsRowVersion (concurrency_test.go): a rename onto the role's own current
// name is a legitimate no-op write, not a self-collision against
// UNIQUE (tenant_id, name) — Postgres's uniqueness check never compares a row
// against itself, only against every OTHER row.
func TestUpdateRoleNameToOwnCurrentNameIsNotAnError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	r, err := s.CreateRole(ctx, tn.ID, "noop-name")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	got, err := s.UpdateRoleName(ctx, tn.ID, r.ID, "noop-name", r.RowVersion)
	if err != nil {
		t.Fatalf("rename to own current name: want success, got %v", err)
	}
	if got.Name != "noop-name" {
		t.Fatalf("Name = %q, want noop-name", got.Name)
	}
	if got.RowVersion <= r.RowVersion {
		t.Fatalf("RowVersion = %d, want > %d (a no-op write still bumps, per the trigger)", got.RowVersion, r.RowVersion)
	}
}

// TestDeleteRoleReportsTheCascadesNobodyWasCounting is A10's reproduction,
// kept as the gate. role carries SEVEN inbound foreign keys across FIVE child
// tables, every one ON DELETE CASCADE, and until this test existed
// RevokedGrants described two of them. Migrations 00020 (virtual_key) and
// 00022 (usage_daily, role_quota) each added a cascading child with the whole
// suite green, because the only test named for this property
// (TestDeleteRoleReportsWhatTheCascadeRevoked, above) seeds exactly the two
// children that were already reported.
//
// Seeded through raw SQL rather than CreateVirtualKey / CreateRoleQuota /
// IncrementUsage, and that is deliberate rather than lazy: those three
// constructors live in virtual_key.ee.go and usage.ee.go, so naming them from
// a shared _test.go file would not compile in the generated Community tree
// (internal/communitygen drops every *.ee.go). DeleteRole and RevokedGrants
// are SHARED code that both editions run, the three child tables exist in
// both editions because migrations are never edition-split, and a gate over
// shared behaviour must run in both editions. What is under test here is the
// FK cascade and what DeleteRole reports about it, not how a key is minted.
func TestDeleteRoleReportsTheCascadesNobodyWasCounting(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "cascade-audit-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	srv, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "cascade-audit-srv", Transport: "http",
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
	art, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "cascade-audit-art", Visibility: "role",
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

	// Three robot credentials capped by this role. Inserted out of
	// client_id order so the ORDER BY in the report is doing real work.
	for _, cid := range []string{"orbeat-vk-nightly", "orbeat-vk-ci", "orbeat-vk-release"} {
		if _, err := s.db.Exec(ctx, `
			INSERT INTO virtual_key (tenant_id, client_id, role_id, name)
			VALUES ($1, $2, $3, $4)`, tn.ID, cid, role.ID, "key "+cid); err != nil {
			t.Fatalf("insert virtual key %s: %v", cid, err)
		}
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO role_quota (tenant_id, role_id, monthly_calls)
		VALUES ($1, $2, 50000)`, tn.ID, role.ID); err != nil {
		t.Fatalf("insert role quota: %v", err)
	}
	// Two metering buckets, 7 + 11 = 18 attributed calls.
	for i, calls := range []int64{7, 11} {
		if _, err := s.db.Exec(ctx, `
			INSERT INTO usage_daily (tenant_id, day, subject, server_id, tool, role_id, calls)
			VALUES ($1, DATE '2026-08-01' + $2::int, 'orbeat-vk-ci', $3, 'read', $4, $5)`,
			tn.ID, i, srv.ID, role.ID, calls); err != nil {
			t.Fatalf("insert usage row %d: %v", i, err)
		}
	}

	got, err := s.DeleteRole(ctx, tn.ID, role.ID)
	if err != nil {
		t.Fatalf("delete role: %v", err)
	}

	if got.Entitlements != 1 || got.ArtifactEntitlements != 1 {
		t.Errorf("revoked = %d entitlements / %d artifact entitlements, want 1/1",
			got.Entitlements, got.ArtifactEntitlements)
	}
	if got.VirtualKeys != 3 {
		t.Errorf("VirtualKeys = %d, want 3", got.VirtualKeys)
	}
	// The client_id SET, not its length: these are the only handles an
	// operator has on the Keycloak clients this DELETE just orphaned.
	wantIDs := []string{"orbeat-vk-ci", "orbeat-vk-nightly", "orbeat-vk-release"}
	if !slices.Equal(got.VirtualKeyClientIDs, wantIDs) {
		t.Errorf("VirtualKeyClientIDs = %v, want %v", got.VirtualKeyClientIDs, wantIDs)
	}
	if got.UsageRows != 2 {
		t.Errorf("UsageRows = %d, want 2", got.UsageRows)
	}
	if got.UsageCalls != 18 {
		t.Errorf("UsageCalls = %d, want 18", got.UsageCalls)
	}
	if got.QuotaMonthlyCalls == nil {
		t.Errorf("QuotaMonthlyCalls = nil, want 50000 (the role carried a quota row and it was destroyed)")
	} else if *got.QuotaMonthlyCalls != 50000 {
		t.Errorf("QuotaMonthlyCalls = %d, want 50000", *got.QuotaMonthlyCalls)
	}

	// The cascade actually fired on all three: a report is only worth
	// anything if the rows really went away.
	for _, table := range []string{"virtual_key", "role_quota", "usage_daily"} {
		var n int
		if err := s.db.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE tenant_id = $1 AND role_id = $2`,
			tn.ID, role.ID).Scan(&n); err != nil {
			t.Fatalf("count %s after delete: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d row(s) for the deleted role", table, n)
		}
	}
}

// TestDeleteRoleReportsNoQuotaAsNil pins the other half of the quota field:
// role_quota is UNIQUE (tenant_id, role_id), so a role has at most one quota
// row and usually none. Nil must mean "no quota existed", which is a
// different fact from "a quota of zero was destroyed", and an operator
// re-creating the role needs to tell them apart.
func TestDeleteRoleReportsNoQuotaAsNil(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "no-quota-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	got, err := s.DeleteRole(ctx, tn.ID, role.ID)
	if err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if got.QuotaMonthlyCalls != nil {
		t.Errorf("QuotaMonthlyCalls = %d, want nil for a role that never had a quota", *got.QuotaMonthlyCalls)
	}
	if got.VirtualKeys != 0 || got.UsageRows != 0 || got.UsageCalls != 0 {
		t.Errorf("empty role reported %d keys / %d usage rows / %d calls, want 0/0/0",
			got.VirtualKeys, got.UsageRows, got.UsageCalls)
	}
	// Never nil, matching ServerNames/ArtifactNames: the audit metadata
	// serialises this straight to JSON and a caller must not have to
	// distinguish `null` from `[]`.
	if got.VirtualKeyClientIDs == nil {
		t.Error("VirtualKeyClientIDs = nil, want an empty non-nil slice")
	}
}

// TestRoleForUpdateBlocksUnlockedChildInsert measures the one claim in
// DeleteRole's doc comment that is about Postgres rather than about this
// codebase.
//
// Four of role's five cascading children are inserted by handlers that first
// take a lock on the role row themselves (RoleExistsInTenant's FOR SHARE for
// entitlement, artifact_entitlement and virtual_key; LockRoleForQuotaWrite's
// FOR UPDATE for role_quota), so they serialize against DeleteRole by
// app-level convention and TestDeleteRoleLocksAgainstConcurrentGrantInsert
// already covers that shape. usage_daily has no such caller: IncrementUsage is
// flushed from a background counter that never reads the role row. If nothing
// stopped that flush, DeleteRole's usage numbers would be a before-picture of
// exactly the kind v1.24.0 was written to eliminate.
//
// What stops it is the referential-integrity check Postgres runs on the child
// INSERT, which takes FOR KEY SHARE on the parent row, and FOR KEY SHARE
// conflicts with FOR UPDATE. This test holds DeleteRole's literal lock
// statement open in one real transaction and requires a usage_daily insert in
// another to block on it, then to succeed once the lock goes. It asserts the
// insert has NOT returned while the lock is held, which is the half that
// discriminates: a test that only checked the insert eventually succeeds would
// pass with no lock at all.
func TestRoleForUpdateBlocksUnlockedChildInsert(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "keyshare-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	srv, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "keyshare-srv", Transport: "http",
		EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	pgtx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin locker tx: %v", err)
	}
	defer pgtx.Rollback(ctx)
	// DeleteRole's first statement, verbatim.
	var name string
	if err := pgtx.QueryRow(ctx,
		`SELECT name FROM role WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tn.ID, role.ID).Scan(&name); err != nil {
		t.Fatalf("lock role row: %v", err)
	}

	inserted := make(chan error, 1)
	go func() {
		_, err := s.db.Exec(ctx, `
			INSERT INTO usage_daily (tenant_id, day, subject, server_id, tool, role_id, calls)
			VALUES ($1, DATE '2026-08-01', 'orbeat-vk-bg', $2, 'read', $3, 1)`,
			tn.ID, srv.ID, role.ID)
		inserted <- err
	}()

	waitForBlockedQuery(t, s, "usage_daily", 5*time.Second)
	select {
	case err := <-inserted:
		t.Fatalf("the usage_daily insert completed (err=%v) while the role row was held FOR UPDATE: "+
			"the RI check did not take a conflicting lock, so DeleteRole's usage counts are a racy "+
			"before-picture and its doc comment is wrong", err)
	default:
	}

	if err := pgtx.Rollback(ctx); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	select {
	case err := <-inserted:
		if err != nil {
			t.Fatalf("usage_daily insert after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the usage_daily insert never returned after the lock was released")
	}
}

// TestDeleteRoleVirtualKeyListIsNeverCapped is what
// TestDeleteRoleTruncationFiresOnTheVirtualKeyListAlone became on
// 2026-08-30. That test pinned the OPPOSITE property -- 51 keys report 50
// client_ids and Truncated true -- and it was right to exist while the cap
// applied: it was the only gate on the keyTrunc term, since
// TestDeleteRoleCapsNameListsAndReportsTruncated seeds server grants only.
// Capo's decision removed the cap from this one list rather than the term
// from that expression, so the property it gated is gone and the test is
// rewritten in place rather than deleted: the fixture, the zero-sibling
// discipline and the named 51st id are all still exactly what this needs,
// only the expected answers are inverted.
//
// The reason the cap went is that this list is unlike its two siblings. A
// capped server or artifact list loses nothing -- those rows survive the
// DELETE and the names are one admin-list call away -- while a capped
// client_id list is permanent: the virtual_key row is destroyed rather than
// revoked, so GET /v1/admin/virtual-keys?revoked=true cannot return it (see
// RevokedGrants), and the 51st client_id was written down nowhere else in
// orbeat.
//
// TWO MUTANTS, AND THE SECOND IS WHY Truncated IS ASSERTED HERE AT ALL:
//
//   - Pass MaxGrantNames instead of `uncapped` at DeleteRole's virtual-key
//     read. len(VirtualKeyClientIDs) becomes 50, and both the length
//     assertion and the "the 51st is present" assertion fail.
//   - Put the cap back as a FLAG rather than as a truncation, i.e.
//     `Truncated: srvTrunc || artTrunc || keyCount > MaxGrantNames`. The
//     list stays whole, so every length assertion still passes, and only the
//     Truncated assertion below catches it. Without that assertion the audit
//     record would say `truncated: true` over a complete list, which sends
//     an operator looking for client_ids that are all already in front of
//     them.
//
// The role is given NO server and NO artifact grant, for the same reason the
// old test did: those are the only two terms left in the Truncated
// expression, so with both at zero a true value can only have come from the
// virtual-key list, and the second mutant cannot hide behind a sibling.
func TestDeleteRoleVirtualKeyListIsNeverCapped(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "vk-cap-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	// Zero-padded so client_id's lexicographic ORDER BY is also the numeric
	// one, which is what lets the assertions below name the exact first and
	// last ids rather than only counting them. MaxGrantNames+1 is deliberate
	// and not an arbitrary "lots": it is the smallest fixture that would have
	// truncated under the old cap, so this test fails the moment the cap
	// comes back at any size.
	const total = MaxGrantNames + 1
	for i := 0; i < total; i++ {
		if _, err := s.db.Exec(ctx, `
			INSERT INTO virtual_key (tenant_id, client_id, role_id, name)
			VALUES ($1, $2, $3, $4)`,
			tn.ID, fmt.Sprintf("orbeat-vk-trunc-%03d", i), role.ID, fmt.Sprintf("key %d", i)); err != nil {
			t.Fatalf("insert virtual key %d: %v", i, err)
		}
	}

	got, err := s.DeleteRole(ctx, tn.ID, role.ID)
	if err != nil {
		t.Fatalf("delete role: %v", err)
	}

	if got.VirtualKeys != total {
		t.Errorf("VirtualKeys = %d, want %d", got.VirtualKeys, total)
	}
	if len(got.VirtualKeyClientIDs) != total {
		t.Fatalf("len(VirtualKeyClientIDs) = %d, want %d: this list names Keycloak clients the DELETE "+
			"destroyed, so every id it drops is unrecoverable from anywhere in orbeat",
			len(got.VirtualKeyClientIDs), total)
	}
	// The invariant that replaces the flag this list no longer sets: the
	// count and the list agree, always, so VirtualKeys > len(...) is now a
	// defect rather than an expected truncation (RevokedGrants.Truncated).
	if got.VirtualKeys != len(got.VirtualKeyClientIDs) {
		t.Errorf("VirtualKeys = %d but the list holds %d: the two must always agree now that the list is uncapped",
			got.VirtualKeys, len(got.VirtualKeyClientIDs))
	}
	// Neither sibling list exists on this role, so a true Truncated below
	// could only have come from the virtual-key list.
	if got.Entitlements != 0 || got.ArtifactEntitlements != 0 {
		t.Fatalf("fixture drift: role has %d server and %d artifact grants; this test needs zero of both "+
			"so that Truncated cannot be set by srvTrunc or artTrunc",
			got.Entitlements, got.ArtifactEntitlements)
	}
	if got.Truncated {
		t.Error("Truncated = true over a complete list: the virtual-key list is uncapped and can no longer " +
			"set this flag, and an audit record claiming truncation sends an operator hunting for client_ids " +
			"that are all already in the record")
	}
	// The full window, ends named: the first id, and the one the old cap
	// destroyed.
	if got.VirtualKeyClientIDs[0] != "orbeat-vk-trunc-000" ||
		got.VirtualKeyClientIDs[total-1] != "orbeat-vk-trunc-050" {
		t.Errorf("reported window = [%s .. %s], want [orbeat-vk-trunc-000 .. orbeat-vk-trunc-050]",
			got.VirtualKeyClientIDs[0], got.VirtualKeyClientIDs[total-1])
	}
	if !slices.Contains(got.VirtualKeyClientIDs, "orbeat-vk-trunc-050") {
		t.Error("orbeat-vk-trunc-050 is missing: that is the exact id the old MaxGrantNames cap destroyed, " +
			"and reporting it is the whole point of this change")
	}
}

// TestDeleteRoleLocksBeforeReadingEveryChild gates the ORDER of DeleteRole's
// reads against its own SELECT ... FOR UPDATE for the three children added
// by A10. Hoist the virtual-key, usage and quota reads above the lock and
// the whole of internal/store and internal/api stay green without this test:
// TestDeleteRoleLocksAgainstConcurrentGrantInsert drives only the
// entitlement path, and TestRoleForUpdateBlocksUnlockedChildInsert proves
// the lock CONFLICTS with a child insert without ever proving DeleteRole
// takes it FIRST. That leaves the exact regression A10 is a second instance
// of -- a before-picture reported as the destroyed set -- reachable for
// virtual keys, quota and metering.
//
// The holder transaction below takes RoleExistsInTenant's FOR SHARE lock,
// which is faithful for virtual_key (handleCreateVirtualKey takes it) and
// close enough for role_quota (LockRoleForQuotaWrite takes FOR UPDATE, which
// conflicts at least as hard). For usage_daily it is deliberately NOT a
// model of a real caller: IncrementUsage is flushed by a background counter
// that never touches the role row, and the reason a flush cannot land inside
// DeleteRole's own window is measured separately by
// TestRoleForUpdateBlocksUnlockedChildInsert. Here the holder is only the
// device that commits rows while DeleteRole is blocked, which is what makes
// a read taken before the lock observably wrong.
func TestDeleteRoleLocksBeforeReadingEveryChild(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "lock-order-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	srv, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "lock-order-srv", Transport: "http",
		EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	// Nothing is seeded up front. Every child row this role will ever own is
	// created by the holder BELOW, while DeleteRole is already blocked, so a
	// read taken before the lock sees zero of each and a read taken after
	// sees all of them. The two outcomes cannot be confused.
	pgtx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer pgtx.Rollback(ctx) // no-op once committed below
	holder := &Store{db: pgtx}
	if ok, err := holder.RoleExistsInTenant(ctx, tn.ID, role.ID); err != nil || !ok {
		t.Fatalf("RoleExistsInTenant: ok=%v err=%v", ok, err)
	}

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

	waitForBlockedQuery(t, s, "role", 5*time.Second)

	if _, err := pgtx.Exec(ctx, `
		INSERT INTO virtual_key (tenant_id, client_id, role_id, name)
		VALUES ($1, 'orbeat-vk-raced', $2, 'raced key')`, tn.ID, role.ID); err != nil {
		t.Fatalf("holder insert virtual key: %v", err)
	}
	if _, err := pgtx.Exec(ctx, `
		INSERT INTO role_quota (tenant_id, role_id, monthly_calls)
		VALUES ($1, $2, 4242)`, tn.ID, role.ID); err != nil {
		t.Fatalf("holder insert role quota: %v", err)
	}
	if _, err := pgtx.Exec(ctx, `
		INSERT INTO usage_daily (tenant_id, day, subject, server_id, tool, role_id, calls)
		VALUES ($1, DATE '2026-08-01', 'raced-subject', $2, 'read', $3, 9)`,
		tn.ID, srv.ID, role.ID); err != nil {
		t.Fatalf("holder insert usage row: %v", err)
	}
	if err := pgtx.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("DeleteRole: %v", res.err)
		}
		const hoisted = "read before the SELECT ... FOR UPDATE, so it is a before-picture of what the DELETE then destroyed"
		if res.g.VirtualKeys != 1 {
			t.Errorf("VirtualKeys = %d, want 1: the key committed while DeleteRole was blocked was %s",
				res.g.VirtualKeys, hoisted)
		}
		if !slices.Equal(res.g.VirtualKeyClientIDs, []string{"orbeat-vk-raced"}) {
			t.Errorf("VirtualKeyClientIDs = %v, want [orbeat-vk-raced]: the only handle on the Keycloak client "+
				"this DELETE orphaned was %s", res.g.VirtualKeyClientIDs, hoisted)
		}
		if res.g.UsageRows != 1 || res.g.UsageCalls != 9 {
			t.Errorf("UsageRows/UsageCalls = %d/%d, want 1/9: the metering committed while DeleteRole was blocked was %s",
				res.g.UsageRows, res.g.UsageCalls, hoisted)
		}
		if res.g.QuotaMonthlyCalls == nil {
			t.Errorf("QuotaMonthlyCalls = nil, want 4242: the quota committed while DeleteRole was blocked was %s", hoisted)
		} else if *res.g.QuotaMonthlyCalls != 4242 {
			t.Errorf("QuotaMonthlyCalls = %d, want 4242", *res.g.QuotaMonthlyCalls)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteRole did not return after the holder committed")
	}

	// The rows really were destroyed, so the numbers above describe a
	// cascade that happened rather than one that was merely reported.
	for _, table := range []string{"virtual_key", "role_quota", "usage_daily"} {
		var n int
		if err := s.db.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE tenant_id = $1 AND role_id = $2`,
			tn.ID, role.ID).Scan(&n); err != nil {
			t.Fatalf("count %s after delete: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d row(s) for the deleted role", table, n)
		}
	}
}
