package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

func TestArtifactEntitlementCreateListDelete(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)
	role, _ := st.CreateRole(ctx, tn.ID, "sec")
	art, _ := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "sec-skill",
		Content: "---\nname: sec-skill\ndescription: d\n---\nx", Visibility: "role",
	})

	// Create → 201 + audit
	rec := httptest.NewRecorder()
	srv.handleCreateArtifactEntitlement(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifact-entitlements",
		map[string]any{"roleId": role.ID, "artifactId": art.ID}, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create=%d body=%s", rec.Code, rec.Body)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("missing id")
	}
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if len(evs) != 1 || evs[0].Action != "artifact_entitlement.create" {
		t.Fatalf("expected artifact_entitlement.create audit, got %+v", evs)
	}

	// List → one
	lrec := httptest.NewRecorder()
	srv.handleListArtifactEntitlements(lrec, adminReq(ctx, http.MethodGet, "/v1/admin/artifact-entitlements", nil, tn))
	var body struct {
		ArtifactEntitlements []map[string]any `json:"artifactEntitlements"`
	}
	_ = json.Unmarshal(lrec.Body.Bytes(), &body)
	if len(body.ArtifactEntitlements) != 1 {
		t.Fatalf("list=%+v", body.ArtifactEntitlements)
	}

	// Delete → 204
	drec := httptest.NewRecorder()
	dreq := adminReq(ctx, http.MethodDelete, "/v1/admin/artifact-entitlements/"+id, nil, tn)
	dreq.SetPathValue("id", id)
	srv.handleDeleteArtifactEntitlement(drec, dreq)
	if drec.Code != http.StatusNoContent {
		t.Fatalf("delete=%d", drec.Code)
	}

	// Delete again (now-unknown id) → 404 (store.ErrNotFound → fail() → 404).
	drec2 := httptest.NewRecorder()
	dreq2 := adminReq(ctx, http.MethodDelete, "/v1/admin/artifact-entitlements/"+id, nil, tn)
	dreq2.SetPathValue("id", id)
	srv.handleDeleteArtifactEntitlement(drec2, dreq2)
	if drec2.Code != http.StatusNotFound {
		t.Fatalf("delete unknown=%d, want 404", drec2.Code)
	}
}

// TestAdminDeleteArtifactEntitlementMalformedIDIs404 proves a non-UUID {id}
// maps to 404, not a 500 leaking a raw Postgres invalid_text_representation
// error (audit B2c) — mirrors the entitlement-delete sibling fix.
func TestAdminDeleteArtifactEntitlementMalformedIDIs404(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodDelete, "/v1/admin/artifact-entitlements/not-a-uuid", nil, tn)
	req.SetPathValue("id", "not-a-uuid")
	srv.handleDeleteArtifactEntitlement(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id delete = %d, want 404, body = %s", rec.Code, rec.Body)
	}
}

func TestArtifactEntitlementForeignRoleIs400(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)
	// An artifact in OUR tenant, but a role belonging to ANOTHER tenant.
	art, _ := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "sec-skill",
		Content: "---\nname: sec-skill\ndescription: d\n---\nx", Visibility: "role",
	})
	other, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("ae-role-other-%d", time.Now().UnixNano()))
	foreignRole, _ := st.CreateRole(ctx, other.ID, "intruder")

	rec := httptest.NewRecorder()
	srv.handleCreateArtifactEntitlement(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifact-entitlements",
		map[string]any{"roleId": foreignRole.ID, "artifactId": art.ID}, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("foreign role=%d, want 400", rec.Code)
	}
	// No entitlement, no audit (validation precedes auditedTx).
	if ents, _ := st.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0); len(ents) != 0 {
		t.Fatalf("must not create cross-tenant entitlement: %+v", ents)
	}
	if evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10); len(evs) != 0 {
		t.Fatalf("no audit expected on rejected entitlement, got %+v", evs)
	}
}

func TestArtifactEntitlementForeignArtifactIs400(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)
	role, _ := st.CreateRole(ctx, tn.ID, "sec")
	other, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("ae-other-%d", time.Now().UnixNano()))
	foreign, _ := st.CreateArtifact(ctx, store.Artifact{
		TenantID: other.ID, Type: "skill", Name: "foreign",
		Content: "---\nname: foreign\ndescription: d\n---\nx", Visibility: "role",
	})
	rec := httptest.NewRecorder()
	srv.handleCreateArtifactEntitlement(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifact-entitlements",
		map[string]any{"roleId": role.ID, "artifactId": foreign.ID}, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("foreign artifact=%d, want 400", rec.Code)
	}
	// No entitlement, no audit (validation precedes auditedTx).
	if ents, _ := st.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0); len(ents) != 0 {
		t.Fatalf("must not create cross-tenant entitlement: %+v", ents)
	}
	if evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10); len(evs) != 0 {
		t.Fatalf("no audit expected on rejected entitlement, got %+v", evs)
	}
}
