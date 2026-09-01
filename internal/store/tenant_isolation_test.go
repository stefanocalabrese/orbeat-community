package store

import (
	"context"
	"errors"
	"testing"
)

// TestCrossTenantIsolation verifies that tenant-scoped mutations cannot touch
// another tenant's rows — the core multi-tenancy security seam.
func TestCrossTenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tenantA := mustTenant(t, s)
	tenantB := mustTenant(t, s)

	// --- set up tenant A's data ---

	srvA, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID:          tenantA.ID,
		Name:              "srv-a",
		Transport:         "http",
		EndpointOrCommand: "https://mcp.example/a",
		Status:            "active",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer(A): %v", err)
	}

	roleA, err := s.CreateRole(ctx, tenantA.ID, "role-a")
	if err != nil {
		t.Fatalf("CreateRole(A): %v", err)
	}

	entA, err := s.CreateEntitlement(ctx, Entitlement{
		TenantID:    tenantA.ID,
		RoleID:      roleA.ID,
		MCPServerID: srvA.ID,
		Permissions: []string{"read"},
	})
	if err != nil {
		t.Fatalf("CreateEntitlement(A): %v", err)
	}

	artA, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tenantA.ID, Type: "skill", Name: "art-a",
		Content: "---\nname: art-a\ndescription: d\n---\nORIGINAL",
	})
	if err != nil {
		t.Fatalf("CreateArtifact(A): %v", err)
	}

	// --- cross-tenant mutation attempts using tenant B's ID ---

	// UpdateArtifact must report the SAME outcome as UpdateMCPServer below
	// (ErrNotFound, not ErrVersionMismatch) for a cross-tenant id — both are
	// "this row is invisible to you", never "you raced someone." Getting this
	// wrong would map to HTTP 412 instead of 404 once the API layer wires up
	// ErrVersionMismatch (spec §5.2).
	t.Run("UpdateArtifact cross-tenant rejected", func(t *testing.T) {
		_, err := s.UpdateArtifact(ctx, Artifact{
			TenantID: tenantB.ID, // wrong tenant
			ID:       artA.ID,    // tenant A's artifact
			Type:     "skill", Name: "hacked",
			Content: "---\nname: hacked\ndescription: d\n---\nHACKED",
		}, artA.RowVersion)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("UpdateArtifact with wrong tenant ID: want ErrNotFound, got %v", err)
		}
	})

	t.Run("UpdateMCPServer cross-tenant rejected", func(t *testing.T) {
		_, err := s.UpdateMCPServer(ctx, MCPServer{
			TenantID:          tenantB.ID, // wrong tenant
			ID:                srvA.ID,    // tenant A's server
			Name:              "hacked",
			Transport:         "http",
			EndpointOrCommand: "https://evil.example",
			Status:            "active",
		}, nil, nil)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("UpdateMCPServer with wrong tenant ID: want ErrNotFound, got %v", err)
		}
	})

	t.Run("DeleteMCPServer cross-tenant rejected", func(t *testing.T) {
		err := s.DeleteMCPServer(ctx, tenantB.ID, srvA.ID)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("DeleteMCPServer with wrong tenant ID: want ErrNotFound, got %v", err)
		}
	})

	t.Run("DeleteEntitlement cross-tenant rejected", func(t *testing.T) {
		err := s.DeleteEntitlement(ctx, tenantB.ID, entA.ID)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("DeleteEntitlement with wrong tenant ID: want ErrNotFound, got %v", err)
		}
	})

	// --- confirm tenant A's data is untouched ---

	t.Run("srvA still exists under tenant A", func(t *testing.T) {
		got, err := s.GetMCPServer(ctx, tenantA.ID, srvA.ID)
		if err != nil {
			t.Fatalf("GetMCPServer(srvA) after cross-tenant attempts: %v", err)
		}
		if got.Name != "srv-a" {
			t.Fatalf("srvA mutated: got Name=%q, want %q", got.Name, "srv-a")
		}
	})

	t.Run("artA still exists under tenant A, unmutated", func(t *testing.T) {
		got, err := s.GetArtifact(ctx, tenantA.ID, artA.ID)
		if err != nil {
			t.Fatalf("GetArtifact(artA) after cross-tenant attempts: %v", err)
		}
		if got.Name != "art-a" || got.Content != "---\nname: art-a\ndescription: d\n---\nORIGINAL" {
			t.Fatalf("artA mutated by a rejected cross-tenant update: %+v", got)
		}
	})

	t.Run("entA still exists under tenant A", func(t *testing.T) {
		ents, err := s.ListEntitlementsPage(ctx, tenantA.ID, nil, 0, false)
		if err != nil {
			t.Fatalf("ListEntitlementsPage(A): %v", err)
		}
		found := false
		for _, e := range ents {
			if e.ID == entA.ID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("entA not found after cross-tenant delete attempts; ents=%+v", ents)
		}
	})
}
