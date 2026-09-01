package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// seedEntitlement creates a role, a server and a grant, returning the grant.
func seedEntitlement(t *testing.T, st *store.Store, tn store.Tenant, tools []string) store.Entitlement {
	t.Helper()
	ctx := context.Background()
	role, err := st.CreateRole(ctx, tn.ID, fmt.Sprintf("ent-role-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	srv, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: fmt.Sprintf("ent-srv-%d", time.Now().UnixNano()),
		Transport: "http", EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	e, err := st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID, AllowedTools: tools,
	})
	if err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	return e
}

func getEntitlement(t *testing.T, srv *Server, tn store.Tenant, id string) (int, string, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := adminReq(context.Background(), http.MethodGet, "/v1/admin/entitlements/"+id, nil, tn)
	req.SetPathValue("id", id)
	srv.handleGetEntitlement(rec, req)
	var body map[string]any
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return rec.Code, rec.Header().Get("ETag"), body
}

func putEntitlement(t *testing.T, srv *Server, tn store.Tenant, id, ifMatch string, in map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := adminReq(context.Background(), http.MethodPut, "/v1/admin/entitlements/"+id, in, tn)
	req.SetPathValue("id", id)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	srv.handleUpdateEntitlement(rec, req)
	return rec
}

// TestUpdateEntitlementReplacesAllowedTools is the capability itself: editing a
// grant's tool list without delete-and-recreate, which was the only way before.
func TestUpdateEntitlementReplacesAllowedTools(t *testing.T) {
	srv, st, tn := newAdminServer(t)
	e := seedEntitlement(t, st, tn, []string{"echo"})

	code, tag, _ := getEntitlement(t, srv, tn, e.ID)
	if code != http.StatusOK || tag == "" {
		t.Fatalf("GET = %d, etag %q: an update has no way to obtain its precondition", code, tag)
	}

	rec := putEntitlement(t, srv, tn, e.ID, tag, map[string]any{"allowedTools": []string{"echo", "search"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body %s", rec.Code, rec.Body)
	}
	var out entitlementDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Join(out.AllowedTools, ",") != "echo,search" {
		t.Fatalf("allowedTools = %v, want [echo search]", out.AllowedTools)
	}
	// Read back through the store, not the response: a handler that returned
	// the input it was given would satisfy the assertion above.
	stored, err := st.GetEntitlement(context.Background(), tn.ID, e.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if strings.Join(stored.AllowedTools, ",") != "echo,search" {
		t.Fatalf("stored allowedTools = %v, want [echo search]", stored.AllowedTools)
	}
	if rec.Header().Get("ETag") == tag {
		t.Fatal("the response carried the OLD ETag, so a client would send a stale precondition next time")
	}
}

// TestUpdateEntitlementRequiresIfMatch pins the precondition contract this
// route inherits from v1.23.0: absent is 428, stale is 412, and neither may
// mutate anything.
func TestUpdateEntitlementRequiresIfMatch(t *testing.T) {
	srv, st, tn := newAdminServer(t)
	e := seedEntitlement(t, st, tn, []string{"echo"})
	_, tag, _ := getEntitlement(t, srv, tn, e.ID)

	if rec := putEntitlement(t, srv, tn, e.ID, "", map[string]any{"allowedTools": []string{"nope"}}); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match = %d, want 428", rec.Code)
	}
	if rec := putEntitlement(t, srv, tn, e.ID, `"999"`, map[string]any{"allowedTools": []string{"nope"}}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match = %d, want 412", rec.Code)
	}
	// Neither rejection may have written anything.
	stored, err := st.GetEntitlement(context.Background(), tn.ID, e.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if strings.Join(stored.AllowedTools, ",") != "echo" {
		t.Fatalf("a refused update mutated the grant: %v", stored.AllowedTools)
	}
	// The valid one still works, which is what stops the two assertions above
	// passing on a route that rejects everything.
	if rec := putEntitlement(t, srv, tn, e.ID, tag, map[string]any{"allowedTools": []string{"echo", "list"}}); rec.Code != http.StatusOK {
		t.Fatalf("valid If-Match = %d, want 200", rec.Code)
	}
}

// TestUpdateEntitlementCannotRepointTheGrant is the security half. A PUT
// carrying a different roleId or mcpServerId must not move the grant: that
// would hand access to another principal under an audit action that says
// "update", and the trail would describe an edit rather than a revoke plus a
// grant.
func TestUpdateEntitlementCannotRepointTheGrant(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	e := seedEntitlement(t, st, tn, []string{"echo"})
	other := seedEntitlement(t, st, tn, []string{"other"})
	_, tag, _ := getEntitlement(t, srv, tn, e.ID)

	rec := putEntitlement(t, srv, tn, e.ID, tag, map[string]any{
		"allowedTools": []string{"echo"},
		"roleId":       other.RoleID,
		"mcpServerId":  other.MCPServerID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body %s", rec.Code, rec.Body)
	}
	stored, err := st.GetEntitlement(ctx, tn.ID, e.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if stored.RoleID != e.RoleID {
		t.Fatalf("the grant was repointed to role %s; access moved between principals under an update", stored.RoleID)
	}
	if stored.MCPServerID != e.MCPServerID {
		t.Fatalf("the grant was repointed to server %s", stored.MCPServerID)
	}
}

// TestUpdateEntitlementAuditsBothOutcomes pins that the authorization surface
// leaves a trail in both directions: an allow on success and a DENY on a stale
// precondition. The deny arm is the one that rots silently, because nothing a
// client sees depends on it.
func TestUpdateEntitlementAuditsBothOutcomes(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	e := seedEntitlement(t, st, tn, []string{"echo"})
	_, tag, _ := getEntitlement(t, srv, tn, e.ID)

	if rec := putEntitlement(t, srv, tn, e.ID, tag, map[string]any{"allowedTools": []string{"echo", "b"}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d", rec.Code)
	}
	if rec := putEntitlement(t, srv, tn, e.ID, `"1"`, map[string]any{"allowedTools": []string{"c"}}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale PUT = %d, want 412", rec.Code)
	}

	evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 100)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var allow, deny int
	for _, ev := range evs {
		if ev.Action != "entitlement.update" || ev.Target != e.ID {
			continue
		}
		switch ev.Decision {
		case "allow":
			allow++
		case "deny":
			deny++
		}
	}
	if allow != 1 {
		t.Fatalf("entitlement.update allow events = %d, want 1", allow)
	}
	if deny != 1 {
		t.Fatalf("entitlement.update deny events = %d, want 1 (a rejected mutation on an authorization surface must leave a trace)", deny)
	}
}
