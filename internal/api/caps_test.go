package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// TestCheckServerActiveCapBothBoundaries is the decisive gate for the server
// cap (docs/plans/orbeat-community-caps-2026-08-19.md Task 4): it must fire
// at N+1 and NOT at N. A small limit (2) is used rather than the real
// Community number (10). The boundary arithmetic under test does not
// depend on which number is configured, and 10 is unreachable from this
// repo's own Enterprise build regardless (see limits.ee_test.go's
// TestCommunityLimitsIsUnlimitedInThisBuild, which is Enterprise-only and so
// is absent from a generated Community tree).
//
// Red-proven by hand (not committed): changing `current >= max` to
// `current > max` in checkServerActiveCap (caps.go) makes the n=2 case
// below wrongly succeed; changing it to `current >= max-1` makes the n=1
// case wrongly fail. Both mutations were applied, observed to fail exactly
// the assertion they should, and reverted.
func TestCheckServerActiveCapBothBoundaries(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.limits = editionLimits{Servers: 2}

	mk := func(name string) store.MCPServer {
		t.Helper()
		m, err := st.CreateMCPServer(ctx, store.MCPServer{
			TenantID: tn.ID, Name: name, Transport: "http",
			EndpointOrCommand: "https://x", Status: "active",
		})
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return m
	}

	// n=0 active < limit=2: allowed.
	if err := srv.checkServerActiveCap(ctx, tn.ID, "", "active"); err != nil {
		t.Fatalf("first active server (n=0) should be allowed, got %v", err)
	}
	mk("s1")

	// n=1 active < limit=2: allowed (the "at N" boundary, the cap-th
	// creation itself must never be blocked).
	if err := srv.checkServerActiveCap(ctx, tn.ID, "", "active"); err != nil {
		t.Fatalf("second active server (n=1 < limit=2) should be allowed, got %v", err)
	}
	mk("s2")

	// n=2 active == limit=2: rejected (the "N+1" boundary).
	err := srv.checkServerActiveCap(ctx, tn.ID, "", "active")
	var lErr limitError
	if !errors.As(err, &lErr) {
		t.Fatalf("third active server (n=2 >= limit=2) should be rejected with limitError, got %v", err)
	}
	if lErr.Resource != "servers" || lErr.Max != 2 || lErr.Current != 2 {
		t.Fatalf("limitError = %+v, want {servers 2 2 _}", lErr)
	}

	// A "disabled" create is never capped, even once the active cap is hit.
	if err := srv.checkServerActiveCap(ctx, tn.ID, "", "disabled"); err != nil {
		t.Fatalf("a disabled server must never be capped: %v", err)
	}
}

// TestCheckServerActiveCapExcludesSelfOnUpdate proves updating an
// ALREADY-active server (its other fields, same status) never counts itself
// against its own cap, while activating a DIFFERENT server at the same cap
// is still rejected. The excludeID parameter is the mechanism, and this is
// what pins it actually works.
func TestCheckServerActiveCapExcludesSelfOnUpdate(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.limits = editionLimits{Servers: 1}

	a, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "a", Transport: "http", EndpointOrCommand: "https://a", Status: "active",
	})
	if err != nil {
		t.Fatalf("seed a: %v", err)
	}

	// At cap (1 active, limit=1): re-saving the server that IS the cap must
	// not block itself.
	if err := srv.checkServerActiveCap(ctx, tn.ID, a.ID, "active"); err != nil {
		t.Fatalf("updating the server that IS the cap must not block itself: %v", err)
	}

	// Activating a DIFFERENT server while at cap must still be rejected.
	b, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "b", Transport: "http", EndpointOrCommand: "https://b", Status: "disabled",
	})
	if err != nil {
		t.Fatalf("seed b: %v", err)
	}
	var lErr limitError
	if err := srv.checkServerActiveCap(ctx, tn.ID, b.ID, "active"); !errors.As(err, &lErr) {
		t.Fatalf("activating a second server at cap should be rejected, got %v", err)
	}
}

// TestCheckRoleCapBothBoundaries mirrors TestCheckServerActiveCapBothBoundaries
// for the role cap (no excludeID: a role has no "inactive" state).
func TestCheckRoleCapBothBoundaries(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.limits = editionLimits{Roles: 2}

	if err := srv.checkRoleCap(ctx, tn.ID); err != nil {
		t.Fatalf("first role (n=0) should be allowed, got %v", err)
	}
	if _, err := st.CreateRole(ctx, tn.ID, "r1"); err != nil {
		t.Fatalf("seed r1: %v", err)
	}

	if err := srv.checkRoleCap(ctx, tn.ID); err != nil {
		t.Fatalf("second role (n=1 < limit=2) should be allowed, got %v", err)
	}
	if _, err := st.CreateRole(ctx, tn.ID, "r2"); err != nil {
		t.Fatalf("seed r2: %v", err)
	}

	err := srv.checkRoleCap(ctx, tn.ID)
	var lErr limitError
	if !errors.As(err, &lErr) {
		t.Fatalf("third role (n=2 >= limit=2) should be rejected with limitError, got %v", err)
	}
	if lErr.Resource != "roles" || lErr.Max != 2 || lErr.Current != 2 {
		t.Fatalf("limitError = %+v, want {roles 2 2 _}", lErr)
	}
}

// TestHandleCreateServerEnforcesCapAtN1 proves checkServerActiveCap is
// actually WIRED into handleCreateServer, not merely present and unused: a
// unit test on checkServerActiveCap alone cannot catch that class of defect.
func TestHandleCreateServerEnforcesCapAtN1(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)
	srv.limits = editionLimits{Servers: 1}

	first := map[string]any{"name": "s1", "transport": "http", "endpointOrCommand": "https://a", "status": "active"}
	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", first, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create (n=0) = %d, want 201, body %s", rec.Code, rec.Body)
	}

	second := map[string]any{"name": "s2", "transport": "http", "endpointOrCommand": "https://b", "status": "active"}
	rec2 := httptest.NewRecorder()
	srv.handleCreateServer(rec2, adminReq(ctx, http.MethodPost, "/v1/admin/servers", second, tn))
	if rec2.Code != http.StatusPaymentRequired {
		t.Fatalf("second create at cap (n=1) = %d, want 402, body %s", rec2.Code, rec2.Body)
	}
	var body struct {
		Limit struct {
			Resource string `json:"resource"`
			Max      int    `json:"max"`
			Current  int    `json:"current"`
			Contact  string `json:"contact"`
		} `json:"limit"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Limit.Resource != "servers" || body.Limit.Max != 1 || body.Limit.Current != 1 {
		t.Fatalf("limit body = %+v", body.Limit)
	}
	if body.Limit.Contact != authz.DefaultContactEmail {
		t.Fatalf("contact = %q, want %q", body.Limit.Contact, authz.DefaultContactEmail)
	}
}

// TestHandleCreateServerCapFreedByDeactivation proves the whole
// create->update->create round trip: deactivating a server via
// handleUpdateServer frees its slot immediately (spec §4), and a
// subsequent create then succeeds where it would otherwise 402.
func TestHandleCreateServerCapFreedByDeactivation(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.limits = editionLimits{Servers: 1}

	first := map[string]any{"name": "s1", "transport": "http", "endpointOrCommand": "https://a", "status": "active"}
	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", first, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID         string `json:"id"`
		RowVersion int64  `json:"rowVersion"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Deactivate s1.
	upd := map[string]any{"name": "s1", "transport": "http", "endpointOrCommand": "https://a", "status": "disabled"}
	urec := httptest.NewRecorder()
	ureq := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+created.ID, upd, tn)
	ureq.SetPathValue("id", created.ID)
	ureq.Header.Set("If-Match", etag(created.RowVersion))
	srv.handleUpdateServer(urec, ureq)
	if urec.Code != http.StatusOK {
		t.Fatalf("deactivate = %d, want 200, body %s", urec.Code, urec.Body)
	}

	// The slot is free: a new active server now succeeds.
	second := map[string]any{"name": "s2", "transport": "http", "endpointOrCommand": "https://b", "status": "active"}
	rec2 := httptest.NewRecorder()
	srv.handleCreateServer(rec2, adminReq(ctx, http.MethodPost, "/v1/admin/servers", second, tn))
	if rec2.Code != http.StatusCreated {
		t.Fatalf("create after deactivation = %d, want 201, body %s", rec2.Code, rec2.Body)
	}

	servers, err := st.ListMCPServersByTenant(ctx, tn.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("expected both servers to exist, got %d", len(servers))
	}
}

// TestHandleUpdateServerEnforcesCapOnActivate proves the cap applies to an
// UPDATE that flips a DIFFERENT server active, not just to create.
func TestHandleUpdateServerEnforcesCapOnActivate(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.limits = editionLimits{Servers: 1}

	_, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "active-one", Transport: "http", EndpointOrCommand: "https://a", Status: "active",
	})
	if err != nil {
		t.Fatalf("seed active-one: %v", err)
	}
	dormant, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "dormant", Transport: "http", EndpointOrCommand: "https://b", Status: "disabled",
	})
	if err != nil {
		t.Fatalf("seed dormant: %v", err)
	}

	activate := map[string]any{"name": "dormant", "transport": "http", "endpointOrCommand": "https://b", "status": "active"}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+dormant.ID, activate, tn)
	req.SetPathValue("id", dormant.ID)
	req.Header.Set("If-Match", etag(dormant.RowVersion))
	srv.handleUpdateServer(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("activating a second server at cap = %d, want 402, body %s", rec.Code, rec.Body)
	}

	m, err := st.GetMCPServer(ctx, tn.ID, dormant.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if m.Status != "disabled" {
		t.Fatalf("rejected activation must not persist, status = %q", m.Status)
	}
}

// TestHandleCreateRoleEnforcesCapAtN1 mirrors
// TestHandleCreateServerEnforcesCapAtN1 for the role cap's wiring.
func TestHandleCreateRoleEnforcesCapAtN1(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)
	srv.limits = editionLimits{Roles: 1}

	rec := httptest.NewRecorder()
	srv.handleCreateRole(rec, adminReq(ctx, http.MethodPost, "/v1/admin/roles", map[string]any{"name": "r1"}, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first role create = %d, want 201, body %s", rec.Code, rec.Body)
	}

	rec2 := httptest.NewRecorder()
	srv.handleCreateRole(rec2, adminReq(ctx, http.MethodPost, "/v1/admin/roles", map[string]any{"name": "r2"}, tn))
	if rec2.Code != http.StatusPaymentRequired {
		t.Fatalf("second role create at cap = %d, want 402, body %s", rec2.Code, rec2.Body)
	}
}

// TestHandleCreateRoleCapFreedByDeletion proves the full
// create-to-cap -> delete -> create round trip: DELETE /v1/admin/roles/{id}
// (shipped v1.24.0) frees the role slot the Community cap consumes.
func TestHandleCreateRoleCapFreedByDeletion(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)
	srv.limits = editionLimits{Roles: 1}

	rec := httptest.NewRecorder()
	srv.handleCreateRole(rec, adminReq(ctx, http.MethodPost, "/v1/admin/roles", map[string]any{"name": "r1"}, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first role create = %d, body %s", rec.Code, rec.Body)
	}
	var created roleDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rec2 := httptest.NewRecorder()
	srv.handleCreateRole(rec2, adminReq(ctx, http.MethodPost, "/v1/admin/roles", map[string]any{"name": "r2"}, tn))
	if rec2.Code != http.StatusPaymentRequired {
		t.Fatalf("second role create at cap = %d, want 402, body %s", rec2.Code, rec2.Body)
	}

	delReq := adminReq(ctx, http.MethodDelete, "/v1/admin/roles/"+created.ID, nil, tn)
	delReq.SetPathValue("id", created.ID)
	delRec := httptest.NewRecorder()
	srv.handleDeleteRole(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete role = %d, want 200, body %s", delRec.Code, delRec.Body)
	}

	rec3 := httptest.NewRecorder()
	srv.handleCreateRole(rec3, adminReq(ctx, http.MethodPost, "/v1/admin/roles", map[string]any{"name": "r3"}, tn))
	if rec3.Code != http.StatusCreated {
		t.Fatalf("create after deletion = %d, want 201, body %s", rec3.Code, rec3.Body)
	}
}
