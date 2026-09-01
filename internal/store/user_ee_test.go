package store

import (
	"context"
	"testing"
)

// seedVirtualKey inserts a virtual_key row via raw SQL to avoid referencing
// the Enterprise-only VirtualKey/CreateVirtualKey symbols from this test file.
// The boundary test (communitygen) flags any EE file that references an
// Enterprise-only symbol, because a generated Community tree would fail to
// compile on that reference. Raw SQL sidesteps the symbol entirely while
// exercising the same database state.
func seedVirtualKey(t *testing.T, ctx context.Context, s *Store, tenantID, clientID, roleID, createdBy string) string {
	t.Helper()
	var id string
	err := s.db.QueryRow(ctx, `
		INSERT INTO virtual_key (tenant_id, client_id, role_id, name, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text`,
		tenantID, clientID, roleID, "b33-key", createdBy,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed virtual key: %v", err)
	}
	return id
}

// vkRevokedAt reads the revoked_at column for a virtual key by client_id.
func vkRevokedAt(t *testing.T, ctx context.Context, s *Store, tenantID, clientID string) bool {
	t.Helper()
	var revokedAt interface{}
	err := s.db.QueryRow(ctx, `SELECT revoked_at FROM virtual_key WHERE tenant_id=$1 AND client_id=$2`,
		tenantID, clientID).Scan(&revokedAt)
	if err != nil {
		t.Fatalf("query vk revoked_at: %v", err)
	}
	return revokedAt != nil
}

// TestDeleteUserRevokesVirtualKeys (B33) verifies that deleting a user also
// revokes every virtual key they created, and that DeletedUser reports the
// revoked IDs so the audit trail can name them.
func TestDeleteUserRevokesVirtualKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "b33-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	user, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|b33", Email: "b33@x.io", DisplayName: "B33 User",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	userID := user.ID // UUID, not subject: CreatedBy is a FK to users.id
	k1ID := seedVirtualKey(t, ctx, s, tn.ID, "orbeat-vk-b33-1", role.ID, userID)
	k2ID := seedVirtualKey(t, ctx, s, tn.ID, "orbeat-vk-b33-2", role.ID, userID)

	// Verify both keys are live before the delete.
	if vkRevokedAt(t, ctx, s, tn.ID, "orbeat-vk-b33-1") {
		t.Fatal("vk1 should be live before delete")
	}
	if vkRevokedAt(t, ctx, s, tn.ID, "orbeat-vk-b33-2") {
		t.Fatal("vk2 should be live before delete")
	}

	// Delete the user.
	deleted, err := s.DeleteUser(ctx, tn.ID, user.ID)
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	// B33: both keys must be revoked.
	if !vkRevokedAt(t, ctx, s, tn.ID, "orbeat-vk-b33-1") {
		t.Error("vk1 still live after creator user deleted (B33: key outlives its creator)")
	}
	if !vkRevokedAt(t, ctx, s, tn.ID, "orbeat-vk-b33-2") {
		t.Error("vk2 still live after creator user deleted (B33: key outlives its creator)")
	}

	// B33: DeletedUser must report the revoked key IDs.
	if len(deleted.RevokedVirtualKeyIDs) != 2 {
		t.Fatalf("RevokedVirtualKeyIDs = %d, want 2 (B33: audit trail missing)", len(deleted.RevokedVirtualKeyIDs))
	}
	idSet := map[string]bool{k1ID: true, k2ID: true}
	for _, rid := range deleted.RevokedVirtualKeyIDs {
		if !idSet[rid] {
			t.Errorf("RevokedVirtualKeyIDs contains unknown id %q (B33)", rid)
		}
		delete(idSet, rid)
	}
	if len(idSet) != 0 {
		t.Errorf("RevokedVirtualKeyIDs missing ids: %v (B33)", idSet)
	}

	// B33 mutant: if the revoke loop is removed, both keys stay live and
	// RevokedVirtualKeyIDs stays empty. The test must fail for that mutant.
}

// TestDeleteUserRevokesVirtualKeysNone (B33) verifies that a user with no
// virtual keys deletes cleanly and reports an empty (not nil) list.
func TestDeleteUserRevokesVirtualKeysNone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	user, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|b33none", Email: "b33n@x.io",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	deleted, err := s.DeleteUser(ctx, tn.ID, user.ID)
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	if len(deleted.RevokedVirtualKeyIDs) != 0 {
		t.Errorf("RevokedVirtualKeyIDs = %d, want 0 for user with no keys (B33)", len(deleted.RevokedVirtualKeyIDs))
	}
}

// TestDeleteUserRevokesOnlyOwnKeys (B33) verifies that deleting a user only
// revokes keys they created, not keys created by others.
func TestDeleteUserRevokesOnlyOwnKeys(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	role, err := s.CreateRole(ctx, tn.ID, "b33shared")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	userA, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|b33a", Email: "b33a@x.io",
	})
	if err != nil {
		t.Fatalf("upsert userA: %v", err)
	}
	userB, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|b33b", Email: "b33b@x.io",
	})
	if err != nil {
		t.Fatalf("upsert userB: %v", err)
	}

	keyAID := seedVirtualKey(t, ctx, s, tn.ID, "orbeat-vk-b33a", role.ID, userA.ID)
	seedVirtualKey(t, ctx, s, tn.ID, "orbeat-vk-b33b", role.ID, userB.ID)

	deleted, err := s.DeleteUser(ctx, tn.ID, userA.ID)
	if err != nil {
		t.Fatalf("DeleteUser(userA): %v", err)
	}

	// keyA must be revoked.
	if !vkRevokedAt(t, ctx, s, tn.ID, "orbeat-vk-b33a") {
		t.Error("keyA still live after userA deleted (B33)")
	}

	// keyB must NOT be revoked.
	if vkRevokedAt(t, ctx, s, tn.ID, "orbeat-vk-b33b") {
		t.Error("keyB revoked after userA deleted (B33: should only revoke own keys)")
	}

	// DeletedUser must only report keyA's ID.
	if len(deleted.RevokedVirtualKeyIDs) != 1 || deleted.RevokedVirtualKeyIDs[0] != keyAID {
		t.Errorf("RevokedVirtualKeyIDs = %v, want [%s] (B33)", deleted.RevokedVirtualKeyIDs, keyAID)
	}
}
