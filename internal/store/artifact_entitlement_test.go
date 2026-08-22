package store

import (
	"context"
	"errors"
	"testing"
)

func TestArtifactEntitlement(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	role, _ := st.CreateRole(ctx, tn.ID, "sec")
	other, _ := st.CreateRole(ctx, tn.ID, "other")

	roleArt, _ := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "sec-rev",
		Content: "---\nname: sec-rev\ndescription: d\n---\nbody", Visibility: "role",
	})
	approveArtifact(t, st, tn.ID, roleArt.ID)
	// An org artifact must never surface via entitlement reads even if one existed.
	orgArt, _ := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "org-skill",
		Content: "---\nname: org-skill\ndescription: d\n---\nx",
	})
	approveArtifact(t, st, tn.ID, orgArt.ID)

	ent, err := st.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
		TenantID: tn.ID, RoleID: role.ID, ArtifactID: roleArt.ID,
	})
	if err != nil || ent.ID == "" {
		t.Fatalf("create entitlement: err=%v ent=%+v", err, ent)
	}

	got, err := st.ListEntitledArtifacts(ctx, tn.ID, []string{role.ID})
	if err != nil {
		t.Fatalf("list entitled: %v", err)
	}
	if len(got) != 1 || got[0].Name != "sec-rev" {
		t.Fatalf("want only entitled sec-rev, got %+v", got)
	}

	// SECURITY INVARIANT: even a (mis-issued) entitlement pointing at an org
	// artifact must NOT surface it — the visibility='role' guard in
	// ListEntitledArtifacts excludes it, so an org artifact can never escape via sync.
	wrongEnt, err := st.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
		TenantID: tn.ID, RoleID: role.ID, ArtifactID: orgArt.ID,
	})
	if err != nil {
		t.Fatalf("create wrong entitlement: %v", err)
	}
	guarded, err := st.ListEntitledArtifacts(ctx, tn.ID, []string{role.ID})
	if err != nil {
		t.Fatalf("list entitled (guard): %v", err)
	}
	if len(guarded) != 1 || guarded[0].Name != "sec-rev" {
		t.Fatalf("visibility='role' guard failed: org artifact leaked, got %+v", guarded)
	}
	// Remove the deliberately-wrong entitlement so later tenant counts stay clean.
	if err := st.DeleteArtifactEntitlement(ctx, tn.ID, wrongEnt.ID); err != nil {
		t.Fatalf("delete wrong entitlement: %v", err)
	}

	// A different role sees nothing; empty roleIDs sees nothing (fail-closed).
	if none, _ := st.ListEntitledArtifacts(ctx, tn.ID, []string{other.ID}); len(none) != 0 {
		t.Fatalf("other role should see nothing, got %+v", none)
	}
	if empty, _ := st.ListEntitledArtifacts(ctx, tn.ID, nil); len(empty) != 0 {
		t.Fatalf("empty roleIDs should see nothing, got %+v", empty)
	}

	if ok, _ := st.ArtifactExistsInTenant(ctx, tn.ID, roleArt.ID); !ok {
		t.Fatal("artifact should exist in tenant")
	}

	if ents, _ := st.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0); len(ents) != 1 {
		t.Fatalf("want 1 entitlement, got %+v", ents)
	}
	if err := st.DeleteArtifactEntitlement(ctx, tn.ID, ent.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ents, _ := st.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0); len(ents) != 0 {
		t.Fatalf("entitlement not deleted: %+v", ents)
	}
}

// TestDeleteArtifactEntitlementMalformedIDIsNotFound proves a non-UUID id is
// treated as ErrNotFound (mapping Postgres 22P02 invalid_text_representation),
// not surfaced as a raw driver error that would 500 at the API layer (audit
// B2c) — mirrors TestDeleteEntitlementMalformedIDIsNotFound.
func TestDeleteArtifactEntitlementMalformedIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	if err := st.DeleteArtifactEntitlement(ctx, tn.ID, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteArtifactEntitlement(bad id): want ErrNotFound, got %v", err)
	}
}

func approveArtifact(t *testing.T, s *Store, tenantID, id string) {
	t.Helper()
	if err := s.InTx(context.Background(), func(tx *Store) error {
		if _, e := tx.GetArtifactForUpdate(context.Background(), tenantID, id); e != nil {
			return e
		}
		_, _, e := tx.SetArtifactApproved(context.Background(), tenantID, id, "approver", 0)
		return e
	}); err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
}
