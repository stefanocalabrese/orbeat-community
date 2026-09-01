package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// adminReq builds a request carrying an admin Principal + a ResolvedContext for tn.
func adminReq(ctx context.Context, method, target string, body any, tn store.Tenant) *http.Request {
	var r *http.Request
	if body != nil {
		buf, _ := json.Marshal(body)
		r = httptest.NewRequest(method, target, bytes.NewReader(buf))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	c := auth.WithPrincipal(r.Context(), auth.Principal{Subject: "kc-boss", Email: "boss@x.io", Roles: []string{"orbeat-admin"}})
	c = authz.WithResolved(c, authz.ResolvedContext{TenantID: tn.ID, UserID: "boss-uid", RoleIDs: nil})
	return r.WithContext(c)
}

func newAdminServer(t *testing.T) (*Server, *store.Store, store.Tenant) {
	t.Helper()
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, err := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("admin-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	return New(s, authz.NewResolver(s, tn.Name), nil, nil, nil), s, tn
}

func TestAdminCreateServerOmitsSecretRef(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	in := map[string]any{
		"name": "github", "description": "GH", "transport": "http",
		"endpointOrCommand": "https://api.githubcopilot.com/mcp/",
		// env:, not vault:: the scheme is incidental to what this test checks
		// (the create response never leaks secretRef), and env: is the one
		// scheme registered in every tier (docs/specs/2026-08-19-orbeat-
		// community-repo-generation-design.md §4).
		"secretRef": "env:ORBEAT_UPSTREAM_GITHUB_TOKEN", "status": "active",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name"] != "github" || got["hasSecret"] != true {
		t.Fatalf("unexpected body: %+v", got)
	}
	if _, leaked := got["secretRef"]; leaked {
		t.Fatal("admin create leaked secretRef")
	}
	if _, leaked := got["secret_ref"]; leaked {
		t.Fatal("admin create leaked secret_ref")
	}
	id, _ := got["id"].(string)
	m, err := st.GetMCPServer(ctx, tn.ID, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.SecretRef != "env:ORBEAT_UPSTREAM_GITHUB_TOKEN" || m.TenantID != tn.ID {
		t.Fatalf("bad persisted server: %+v", m)
	}
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if len(evs) != 1 || evs[0].Action != "server.create" || evs[0].Decision != "allow" {
		t.Fatalf("expected one server.create audit, got %+v", evs)
	}
}

// TestAdminUpdateServerReplacesAndAudits proves a full-replace update applies
// every field, bumps row_version, and audits — using an EXPLICIT "" for
// secretRef so the clear is a deliberate part of the request (defect 1,
// 2026-09-01: an update body's tri-state secretRef/tlsCaRef distinguishes
// "not mentioned" from "explicit empty string" — see
// TestAdminUpdateServerOmittedRefsPreserveExisting below for the omitted-key
// case this test used to conflate with a clear).
func TestAdminUpdateServerReplacesAndAudits(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	orig, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "upd", Transport: "http",
		EndpointOrCommand: "https://old", SecretRef: "vault:kv/old#token", Status: "active",
	})

	// Full-replace update that EXPLICITLY clears the secret ("secretRef":"" is
	// present in the body). "disabled" is one of the two valid lifecycle
	// states (active|disabled).
	in := map[string]any{
		"name": "upd", "transport": "http",
		"endpointOrCommand": "https://new", "secretRef": "", "status": "disabled",
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+orig.ID, in, tn)
	req.SetPathValue("id", orig.ID)
	req.Header.Set("If-Match", etag(orig.RowVersion))
	srv.handleUpdateServer(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body)
	}
	if etag := rec.Header().Get("ETag"); etag != `"2"` {
		t.Fatalf("ETag = %q, want %q (row_version bumped by the update)", etag, `"2"`)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["endpointOrCommand"] != "https://new" || got["status"] != "disabled" {
		t.Fatalf("update not applied: %+v", got)
	}
	if rv, ok := got["rowVersion"].(float64); !ok || rv != 2 {
		t.Fatalf("rowVersion = %v, want 2", got["rowVersion"])
	}
	if got["hasSecret"] != false {
		t.Fatalf("hasSecret should be false after the explicit clear: %+v", got)
	}
	if _, leaked := got["secretRef"]; leaked {
		t.Fatal("update leaked secretRef")
	}
	// Persisted state reflects the cleared secret.
	m, _ := st.GetMCPServer(ctx, tn.ID, orig.ID)
	if m.SecretRef != "" || m.EndpointOrCommand != "https://new" {
		t.Fatalf("persisted update wrong: %+v", m)
	}
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if len(evs) != 1 || evs[0].Action != "server.update" || evs[0].Decision != "allow" {
		t.Fatalf("expected one server.update audit, got %+v", evs)
	}
}

// TestAdminUpdateServerOmittedRefsPreserveExisting is the HTTP-layer proof
// for defect 1 (2026-09-01, BREAKING): a PUT body that OMITS secretRef and
// tlsCaRef entirely must leave both exactly as they were, not wipe them —
// which is what the pre-fix contract did, silently, because
// GetMCPServer/toAdminServerDTO never echo either ref back (hasSecret/
// hasTlsCa booleans only), so no read-modify-write client could ever have
// resent the value it was never shown.
func TestAdminUpdateServerOmittedRefsPreserveExisting(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	orig, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "preserve-http", Transport: "http",
		EndpointOrCommand: "https://old",
		SecretRef:         "vault:kv/old#token", TLSCARef: "env:ORBEAT_UPSTREAM_CA",
		Status: "active",
	})

	// Neither secretRef nor tlsCaRef appears in this body at all — a caller
	// updating only the endpoint, exactly the read-modify-write script defect
	// 1 describes.
	in := map[string]any{
		"name": "preserve-http", "transport": "http",
		"endpointOrCommand": "https://new", "status": "active",
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+orig.ID, in, tn)
	req.SetPathValue("id", orig.ID)
	req.Header.Set("If-Match", etag(orig.RowVersion))
	srv.handleUpdateServer(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["endpointOrCommand"] != "https://new" {
		t.Fatalf("the mentioned field must still apply: %+v", got)
	}
	if got["hasSecret"] != true || got["hasTlsCa"] != true {
		t.Fatalf("omitting secretRef/tlsCaRef must preserve both, got hasSecret=%v hasTlsCa=%v",
			got["hasSecret"], got["hasTlsCa"])
	}
	m, err := st.GetMCPServer(ctx, tn.ID, orig.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.SecretRef != "vault:kv/old#token" {
		t.Fatalf("SecretRef = %q, want it preserved unchanged", m.SecretRef)
	}
	if m.TLSCARef != "env:ORBEAT_UPSTREAM_CA" {
		t.Fatalf("TLSCARef = %q, want it preserved unchanged", m.TLSCARef)
	}
}

// TestAdminUpdateServerExplicitEmptyRefsClear is
// TestAdminUpdateServerOmittedRefsPreserveExisting's counterpart: sending an
// EXPLICIT "" for both refs must still clear them, proving the fix adds a
// third state rather than removing the ability to clear at all.
func TestAdminUpdateServerExplicitEmptyRefsClear(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	orig, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "clear-http", Transport: "http",
		EndpointOrCommand: "https://old",
		SecretRef:         "vault:kv/old#token", TLSCARef: "env:ORBEAT_UPSTREAM_CA",
		Status: "active",
	})

	in := map[string]any{
		"name": "clear-http", "transport": "http",
		"endpointOrCommand": "https://old", "secretRef": "", "tlsCaRef": "", "status": "active",
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+orig.ID, in, tn)
	req.SetPathValue("id", orig.ID)
	req.Header.Set("If-Match", etag(orig.RowVersion))
	srv.handleUpdateServer(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body)
	}
	m, err := st.GetMCPServer(ctx, tn.ID, orig.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.SecretRef != "" {
		t.Fatalf("SecretRef = %q, want cleared", m.SecretRef)
	}
	if m.TLSCARef != "" {
		t.Fatalf("TLSCARef = %q, want cleared", m.TLSCARef)
	}
}

// TestAdminUpdateServerReplaceThenOmitPreservesTheReplacedValue chains a
// replace and a later omit: the second PUT must preserve whatever the FIRST
// PUT actually wrote, not some earlier value — proving the tri-state reads
// the live row rather than assuming a caller-supplied "previous" value.
func TestAdminUpdateServerReplaceThenOmitPreservesTheReplacedValue(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	orig, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "chain", Transport: "http",
		EndpointOrCommand: "https://old", SecretRef: "env:ORBEAT_UPSTREAM_OLD_TOKEN", Status: "active",
	})

	replace := map[string]any{
		"name": "chain", "transport": "http",
		"endpointOrCommand": "https://old", "secretRef": "env:ORBEAT_UPSTREAM_NEW_TOKEN", "status": "active",
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+orig.ID, replace, tn)
	req.SetPathValue("id", orig.ID)
	req.Header.Set("If-Match", etag(orig.RowVersion))
	srv.handleUpdateServer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace = %d, body %s", rec.Code, rec.Body)
	}
	var replaced struct {
		RowVersion int64 `json:"rowVersion"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &replaced); err != nil {
		t.Fatalf("decode: %v", err)
	}

	omit := map[string]any{
		"name": "chain", "transport": "http",
		"endpointOrCommand": "https://newer", "status": "active",
	}
	rec2 := httptest.NewRecorder()
	req2 := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+orig.ID, omit, tn)
	req2.SetPathValue("id", orig.ID)
	req2.Header.Set("If-Match", etag(replaced.RowVersion))
	srv.handleUpdateServer(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("omit = %d, body %s", rec2.Code, rec2.Body)
	}

	m, err := st.GetMCPServer(ctx, tn.ID, orig.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.SecretRef != "env:ORBEAT_UPSTREAM_NEW_TOKEN" {
		t.Fatalf("SecretRef = %q, want the FIRST update's value preserved, not the original", m.SecretRef)
	}
}

func TestAdminCreateServerInvalidTransportIs400(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)
	in := map[string]any{
		"name": "bad", "transport": "grpc",
		"endpointOrCommand": "https://x", "status": "active",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid transport = %d, want 400", rec.Code)
	}
}

// TestAdminCreateServerStatusValidation proves only "active"/"disabled" are
// accepted (mirrors the mcp_server_status_check DB constraint); any other
// value — including a case variant — is a 400, not a silent typo that would
// delist the server from the catalog+gateway.
func TestAdminCreateServerStatusValidation(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)
	cases := []struct {
		status string
		want   int
	}{
		{"active", http.StatusCreated},
		{"disabled", http.StatusCreated},
		{"Active", http.StatusBadRequest},
		{"bogus", http.StatusBadRequest},
	}
	for i, tc := range cases {
		in := map[string]any{
			"name": fmt.Sprintf("status-create-%d", i), "transport": "http",
			"endpointOrCommand": "https://x", "status": tc.status,
		}
		rec := httptest.NewRecorder()
		srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
		if rec.Code != tc.want {
			t.Fatalf("status=%q got=%d want=%d body=%s", tc.status, rec.Code, tc.want, rec.Body)
		}
	}
}

// TestAdminUpdateServerStatusValidation mirrors the create-side check for update.
func TestAdminUpdateServerStatusValidation(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	orig, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "status-update", Transport: "http",
		EndpointOrCommand: "https://x", Status: "active",
	})
	cases := []struct {
		status string
		want   int
	}{
		{"disabled", http.StatusOK},
		{"active", http.StatusOK},
		{"Active", http.StatusBadRequest},
		{"bogus", http.StatusBadRequest},
	}
	// version tracks row_version across the loop: the two 200 cases actually
	// write and bump it, so If-Match must track the live value; the two 400
	// cases reject at status validation, before the store is ever touched, so
	// the stale header they carry is irrelevant to what they're proving.
	version := orig.RowVersion
	for _, tc := range cases {
		in := map[string]any{
			"name": "status-update", "transport": "http",
			"endpointOrCommand": "https://x", "status": tc.status,
		}
		rec := httptest.NewRecorder()
		req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+orig.ID, in, tn)
		req.SetPathValue("id", orig.ID)
		req.Header.Set("If-Match", etag(version))
		srv.handleUpdateServer(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("status=%q got=%d want=%d body=%s", tc.status, rec.Code, tc.want, rec.Body)
		}
		if tc.want == http.StatusOK {
			var got struct {
				RowVersion int64 `json:"rowVersion"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			version = got.RowVersion
		}
	}
}

// TestAdminCreateServerSlugCollisionIs409 pins the api-side prevention of the
// gateway slug collision (audit G3): the DB is unique on the RAW name only,
// but the gateway routes by naming.Slugify(name), which is lossy — "My Server"
// and "my-server" share a slug, so admitting both silently misroutes per-call
// RBAC. Creating a server whose slug collides with an existing one must 409.
func TestAdminCreateServerSlugCollisionIs409(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	first := map[string]any{
		"name": "My Server", "transport": "http",
		"endpointOrCommand": "https://a", "status": "active",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", first, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d, body %s", rec.Code, rec.Body)
	}

	for _, name := range []string{"my-server", "MY SERVER!"} {
		in := map[string]any{
			"name": name, "transport": "http",
			"endpointOrCommand": "https://b", "status": "active",
		}
		rec2 := httptest.NewRecorder()
		srv.handleCreateServer(rec2, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
		if rec2.Code != http.StatusConflict {
			t.Fatalf("colliding create %q = %d, want 409, body %s", name, rec2.Code, rec2.Body)
		}
		if !bytes.Contains(rec2.Body.Bytes(), []byte("My Server")) {
			t.Fatalf("409 body should name the colliding server, got %s", rec2.Body)
		}
	}

	servers, _ := st.ListMCPServersByTenant(ctx, tn.ID)
	if len(servers) != 1 {
		t.Fatalf("colliding creates must not persist, servers = %+v", servers)
	}
}

// TestAdminUpdateServerSlugCollisionIs409 mirrors the create-side check for
// update: renaming onto another server's slug is a 409, while keeping the
// server's own name (or a slug-equivalent variant of it) stays a 200 —
// the collision check must exclude self.
func TestAdminUpdateServerSlugCollisionIs409(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	_, _ = st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "alpha", Transport: "http",
		EndpointOrCommand: "https://a", Status: "active",
	})
	beta, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "beta", Transport: "http",
		EndpointOrCommand: "https://b", Status: "active",
	})

	// version tracks beta's row_version: the collision case (409) reads the
	// store (checkServerSlugCollision lists servers to compare slugs) but
	// never reaches the version-checked UPDATE, so it doesn't matter what
	// If-Match the closure sends there — the two successful renames DO reach
	// it and must chain off the live value or they'd themselves spuriously
	// 412.
	version := beta.RowVersion
	update := func(name string) *httptest.ResponseRecorder {
		in := map[string]any{
			"name": name, "transport": "http",
			"endpointOrCommand": "https://b2", "status": "active",
		}
		rec := httptest.NewRecorder()
		req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+beta.ID, in, tn)
		req.SetPathValue("id", beta.ID)
		req.Header.Set("If-Match", etag(version))
		srv.handleUpdateServer(rec, req)
		if rec.Code == http.StatusOK {
			var got struct {
				RowVersion int64 `json:"rowVersion"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			version = got.RowVersion
		}
		return rec
	}

	// Rename onto alpha's slug → 409, and the persisted name is untouched.
	if rec := update("Alpha!"); rec.Code != http.StatusConflict {
		t.Fatalf("update-to-collision = %d, want 409, body %s", rec.Code, rec.Body)
	}
	m, _ := st.GetMCPServer(ctx, tn.ID, beta.ID)
	if m.Name != "beta" {
		t.Fatalf("rejected update must not persist, name = %q", m.Name)
	}

	// Keeping its own name → 200 (the check excludes self).
	if rec := update("beta"); rec.Code != http.StatusOK {
		t.Fatalf("update keeping own name = %d, want 200, body %s", rec.Code, rec.Body)
	}
	// A slug-equivalent variant of its OWN name is also fine.
	if rec := update("Beta!"); rec.Code != http.StatusOK {
		t.Fatalf("update to own slug variant = %d, want 200, body %s", rec.Code, rec.Body)
	}
}

func TestAdminGetServerOtherTenantIs404(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	other, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("other-%d", time.Now().UnixNano()))
	foreign, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: other.ID, Name: "foreign", Transport: "http", EndpointOrCommand: "https://z", Status: "active"})

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodGet, "/v1/admin/servers/"+foreign.ID, nil, tn)
	req.SetPathValue("id", foreign.ID)
	srv.handleGetServer(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get = %d, want 404", rec.Code)
	}
}

// TestAdminGetServerMalformedIDIs404 proves a non-UUID {id} maps to 404, not
// a 500 leaking a raw Postgres invalid_text_representation error.
func TestAdminGetServerMalformedIDIs404(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodGet, "/v1/admin/servers/not-a-uuid", nil, tn)
	req.SetPathValue("id", "not-a-uuid")
	srv.handleGetServer(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id get = %d, want 404, body = %s", rec.Code, rec.Body)
	}
}

// TestAdminUpdateServerMalformedIDIs404 proves the same for update, at the
// HTTP layer. handleUpdateServer's Task-3 optimistic-concurrency stopgap
// getter (which used to precede the store call and would also have caught a
// malformed id itself) is gone as of Task 6 — the request now goes straight
// from checkServerSlugCollision (a Go-side name/slug comparison that never
// touches id) to UpdateMCPServer, so this test IS now direct evidence that
// UpdateMCPServer's own idCastNotFound mapping (internal/store/mcpserver.go)
// is load-bearing at the HTTP layer, not just at the store layer where
// TestMCPServerMalformedIDIsNotFound (internal/store/mcpserver_test.go) pins
// it independently.
func TestAdminUpdateServerMalformedIDIs404(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	in := map[string]any{
		"name": "x", "transport": "http",
		"endpointOrCommand": "https://x", "status": "active",
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/not-a-uuid", in, tn)
	req.SetPathValue("id", "not-a-uuid")
	req.Header.Set("If-Match", `"1"`)
	srv.handleUpdateServer(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id update = %d, want 404, body = %s", rec.Code, rec.Body)
	}
}

// TestAdminDeleteServerMalformedIDIs404 proves the same for delete.
// handleDeleteServer has no preceding getter either.
func TestAdminDeleteServerMalformedIDIs404(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodDelete, "/v1/admin/servers/not-a-uuid", nil, tn)
	req.SetPathValue("id", "not-a-uuid")
	srv.handleDeleteServer(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id delete = %d, want 404, body = %s", rec.Code, rec.Body)
	}
}

// TestAdminUpdateServerRequiresIfMatch drives the REAL router (Task 6 of the
// optimistic-concurrency plan, spec §5/§6.2/§9) — real RS256-token auth, real
// admin-role gate, real mux dispatch (see mwOrderTestIdP,
// middleware_order_test.go) — because the precondition is enforced inside
// handleUpdateServer, but the CORS/role/resolver wiring sits ABOVE it
// (mirroring TestCORSExposesETagOnRealAdminArtifactRoutes's rationale: only
// the router proves the full chain, not a direct handler call like every
// other test in this file).
//
// Subtests run in declared order and share one seeded row: the assertions
// depend on that (the 412 case must run while row_version is still 1; the 200
// case then bumps it to 2, and the malformed-id case runs last since its
// If-Match value no longer matters).
func TestAdminUpdateServerRequiresIfMatch(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tenantName := fmt.Sprintf("ifmatch-servers-%d", time.Now().UnixNano())
	idp := newMWOrderTestIdP(t)
	v, err := auth.NewValidator(ctx, auth.Config{Issuer: idp.srv.URL, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	srv := New(st, authz.NewResolver(st, tenantName), v, nil, nil)
	tok := idp.token(t, "kc-ifmatch-admin", []string{"orbeat-admin"})

	tn, err := st.GetOrCreateTenantByName(ctx, tenantName)
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	orig, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "ifmatch-server", Transport: "http",
		EndpointOrCommand: "https://old", Status: "active",
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	if orig.RowVersion != 1 {
		t.Fatalf("seed RowVersion = %d, want 1", orig.RowVersion)
	}

	put := func(id, name string, ifMatch *string, endpoint string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{
			"name": name, "transport": "http",
			"endpointOrCommand": endpoint, "status": "active",
		})
		req := httptest.NewRequest(http.MethodPut, "/v1/admin/servers/"+id, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		if ifMatch != nil {
			req.Header.Set("If-Match", *ifMatch)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	strp := func(s string) *string { return &s }

	t.Run("no If-Match is 428", func(t *testing.T) {
		rec := put(orig.ID, "ifmatch-server", nil, "https://attempt-no-header")
		if rec.Code != http.StatusPreconditionRequired {
			t.Fatalf("status = %d, want 428, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("If-Match: * is 428", func(t *testing.T) {
		rec := put(orig.ID, "ifmatch-server", strp("*"), "https://attempt-wildcard")
		if rec.Code != http.StatusPreconditionRequired {
			t.Fatalf("status = %d, want 428, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("unquoted If-Match is 400", func(t *testing.T) {
		rec := put(orig.ID, "ifmatch-server", strp("7"), "https://attempt-unquoted")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
		}
	})

	// The decisive case: a stale If-Match must 412 AND must not touch the row.
	// A 412 that still writes is worse than no guard at all.
	t.Run("stale If-Match is 412 and does not mutate", func(t *testing.T) {
		rec := put(orig.ID, "ifmatch-server", strp(`"999"`), "https://attempt-stale")
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("status = %d, want 412, body = %s", rec.Code, rec.Body)
		}
		m, err := st.GetMCPServer(ctx, tn.ID, orig.ID)
		if err != nil {
			t.Fatalf("reread: %v", err)
		}
		if m.EndpointOrCommand != "https://old" {
			t.Fatalf("412 must not mutate the row, endpointOrCommand = %q", m.EndpointOrCommand)
		}
		if m.RowVersion != 1 {
			t.Fatalf("412 must not bump row_version, got %d", m.RowVersion)
		}
		// v1.17.0 finding B1: a deny decision must leave a durable trace, not
		// just a status code. A 412 IS a rejected mutation.
		evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
		if err != nil {
			t.Fatalf("audit list: %v", err)
		}
		found := false
		for _, ev := range evs {
			// reason is asserted too, not just action/decision/target: without
			// it, an unrelated deny on the same action/target (e.g. a future
			// scanner-block reusing "server.update") would satisfy this check
			// just as well, which isn't what a stale If-Match should prove.
			if ev.Action == "server.update" && ev.Decision == "deny" && ev.Target == orig.ID &&
				ev.Metadata["reason"] == "version_mismatch" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a deny-audited server.update for the stale If-Match, got %+v", evs)
		}
	})

	t.Run("current If-Match is 200 with bumped rowVersion and matching ETag", func(t *testing.T) {
		rec := put(orig.ID, "ifmatch-server", strp(`"1"`), "https://attempt-current")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
		}
		var got struct {
			RowVersion int64 `json:"rowVersion"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.RowVersion != 2 {
			t.Fatalf("rowVersion = %d, want 2", got.RowVersion)
		}
		if etag := rec.Header().Get("ETag"); etag != `"2"` {
			t.Fatalf("ETag = %q, want %q", etag, `"2"`)
		}
	})

	// The v1.16.0 class: a malformed path id must still 404, not 500 — even
	// with a well-formed If-Match, and even now that the stopgap precondition
	// read in handleUpdateServer is gone (UpdateMCPServer's own idCastNotFound
	// mapping, inside the CTE, must carry this alone). A name distinct from
	// the seeded row's avoids a 409 from checkServerSlugCollision, which runs
	// (and would legitimately fire) before the store update ever sees the id.
	t.Run("malformed path id with valid If-Match is 404", func(t *testing.T) {
		rec := put("not-a-uuid", "ifmatch-server-probe", strp(`"1"`), "https://attempt-malformed-id")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body)
		}
	})
}

func TestAdminCreateRoleAndList(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	rec := httptest.NewRecorder()
	srv.handleCreateRole(rec, adminReq(ctx, http.MethodPost, "/v1/admin/roles", map[string]any{"name": "orbeat-user"}, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create role = %d, body %s", rec.Code, rec.Body)
	}
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if len(evs) != 1 || evs[0].Action != "role.create" {
		t.Fatalf("expected role.create audit, got %+v", evs)
	}

	lrec := httptest.NewRecorder()
	srv.handleListRoles(lrec, adminReq(ctx, http.MethodGet, "/v1/admin/roles", nil, tn))
	var body struct {
		Roles []map[string]any `json:"roles"`
	}
	_ = json.Unmarshal(lrec.Body.Bytes(), &body)
	if len(body.Roles) != 1 || body.Roles[0]["name"] != "orbeat-user" {
		t.Fatalf("list roles = %+v", body.Roles)
	}
}

// TestCreateRoleRefusesKeycloakBuiltinNames is audit B8's create-half
// red-proof: a role named after either Keycloak built-in must be refused
// with 400, and refused BEFORE anything is persisted (no row, no audit
// event) — never merely "created but harmless somehow".
// TestRefuseKeycloakBuiltinRoleName is the pure, direct unit proof for
// refuseKeycloakBuiltinRoleName (admin_roles.go) — table-driven so the
// prefix rule's exact boundary is pinned independently of any HTTP handler:
// a "default-roles-" prefix match (any realm), the bare prefix by itself,
// the two exact-match sentinels, AND the cases that must NOT match (a
// near-miss missing the hyphen, a differently-cased prefix, and an ordinary
// role name) — a test that only asserted the positive cases could pass on a
// mutant that refuses everything.
func TestRefuseKeycloakBuiltinRoleName(t *testing.T) {
	cases := []struct {
		name    string
		refused bool
	}{
		{"offline_access", true},
		{"uma_authorization", true},
		{"default-roles-orbeat", true},
		{"default-roles-prod", true},
		{"default-roles-some-other-realm-name", true},
		{"default-roles-", true},        // the bare prefix, no realm suffix at all
		{"default-rolesabc", false},     // missing hyphen: not Keycloak's literal
		{"Default-Roles-orbeat", false}, // different case: not Keycloak's literal
		{"orbeat-admin", false},
		{"ci-runner", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refuseKeycloakBuiltinRoleName(tc.name); got != tc.refused {
				t.Fatalf("refuseKeycloakBuiltinRoleName(%q) = %v, want %v", tc.name, got, tc.refused)
			}
		})
	}
}

func TestCreateRoleRefusesKeycloakBuiltinNames(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	for _, name := range []string{"offline_access", "uma_authorization", "default-roles-orbeat"} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.handleCreateRole(rec, adminReq(ctx, http.MethodPost, "/v1/admin/roles", map[string]any{"name": name}, tn))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create role %q = %d, want 400, body %s", name, rec.Code, rec.Body)
			}
			roles, err := st.ListRolesPage(ctx, tn.ID, nil, 0, false, "")
			if err != nil {
				t.Fatalf("list roles: %v", err)
			}
			for _, r := range roles {
				if r.Name == name {
					t.Fatalf("a role named %q was persisted despite the 400", name)
				}
			}
			evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 50)
			if err != nil {
				t.Fatalf("list audit: %v", err)
			}
			for _, ev := range evs {
				if ev.Action == "role.create" && ev.Metadata["name"] == name {
					t.Fatalf("a role.create audit event was recorded for the refused name %q", name)
				}
			}
		})
	}

	// A legitimate role sharing no name with either built-in must still be
	// admitted — otherwise this could be a mutant that refuses everything.
	rec := httptest.NewRecorder()
	srv.handleCreateRole(rec, adminReq(ctx, http.MethodPost, "/v1/admin/roles", map[string]any{"name": "ci-runner"}, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create role %q = %d, want 201, body %s", "ci-runner", rec.Code, rec.Body)
	}
}

// TestUpdateRoleRefusesKeycloakBuiltinNames is TestCreateRoleRefusesKeycloak-
// BuiltinNames' rename-path counterpart — B8's own scope names BOTH create
// and rename, and until this test neither the two exact-match sentinels nor
// the new default-roles- prefix (defect 2, 2026-09-01) had a rename-path
// proof: refuseKeycloakBuiltinRoleName is shared code, but a shared function
// wired into two call sites needs a test AT each call site, since a future
// edit could remove the check from one handler while leaving the other, and
// a create-only test would never notice a rename regression.
//
// s.roleExists is left nil (no realm-role lookup configured) on purpose: the
// refusal in handleUpdateRole runs BEFORE verifyIdpRename is ever called
// (admin_roles.go's own comment on that ordering), so this test exercises
// the refusal in isolation from the IdP-verification path entirely — a
// rename to a refused name must 400 regardless of whether a lookup is
// configured.
func TestUpdateRoleRefusesKeycloakBuiltinNames(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	for _, name := range []string{"offline_access", "uma_authorization", "default-roles-orbeat"} {
		t.Run(name, func(t *testing.T) {
			orig, err := st.CreateRole(ctx, tn.ID, "rename-target-"+name)
			if err != nil {
				t.Fatalf("seed role: %v", err)
			}
			rec := httptest.NewRecorder()
			req := adminReq(ctx, http.MethodPut, "/v1/admin/roles/"+orig.ID,
				map[string]any{"name": name}, tn)
			req.SetPathValue("id", orig.ID)
			req.Header.Set("If-Match", etag(orig.RowVersion))
			srv.handleUpdateRole(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("rename role to %q = %d, want 400, body %s", name, rec.Code, rec.Body)
			}
			m, err := st.GetRole(ctx, tn.ID, orig.ID)
			if err != nil {
				t.Fatalf("get role: %v", err)
			}
			if m.Name == name {
				t.Fatalf("role was renamed to %q despite the 400", name)
			}
			evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 50)
			if err != nil {
				t.Fatalf("list audit: %v", err)
			}
			for _, ev := range evs {
				if ev.Action == "role.rename" && ev.Metadata["to"] == name {
					t.Fatalf("a role.rename audit event was recorded for the refused name %q", name)
				}
			}
		})
	}

	// A legitimate rename target sharing no name with any built-in must
	// still be admitted.
	orig, err := st.CreateRole(ctx, tn.ID, "rename-target-ok")
	if err != nil {
		t.Fatalf("seed role: %v", err)
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/roles/"+orig.ID,
		map[string]any{"name": "ci-runner-2", "idpRenamed": true}, tn)
	req.SetPathValue("id", orig.ID)
	req.Header.Set("If-Match", etag(orig.RowVersion))
	srv.handleUpdateRole(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legitimate rename = %d, want 200, body %s", rec.Code, rec.Body)
	}
}

func TestAdminCreateEntitlementValidatesTenant(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	srvr, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "github", Transport: "http", EndpointOrCommand: "https://x", Status: "active"})

	rec := httptest.NewRecorder()
	srv.handleCreateEntitlement(rec, adminReq(ctx, http.MethodPost, "/v1/admin/entitlements",
		map[string]any{"roleId": role.ID, "mcpServerId": srvr.ID}, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("entitlement create = %d, body %s", rec.Code, rec.Body)
	}

	other, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("ent-other-%d", time.Now().UnixNano()))
	foreign, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: other.ID, Name: "foreign", Transport: "http", EndpointOrCommand: "https://z", Status: "active"})
	brec := httptest.NewRecorder()
	srv.handleCreateEntitlement(brec, adminReq(ctx, http.MethodPost, "/v1/admin/entitlements",
		map[string]any{"roleId": role.ID, "mcpServerId": foreign.ID}, tn))
	if brec.Code != http.StatusBadRequest {
		t.Fatalf("cross-tenant entitlement = %d, want 400", brec.Code)
	}

	drec := httptest.NewRecorder()
	srv.handleListEntitlements(drec, adminReq(ctx, http.MethodGet, "/v1/admin/entitlements", nil, tn))
	var body struct {
		Entitlements []map[string]any `json:"entitlements"`
	}
	_ = json.Unmarshal(drec.Body.Bytes(), &body)
	if len(body.Entitlements) != 1 {
		t.Fatalf("list entitlements = %+v", body.Entitlements)
	}
}

func TestAdminCreateEntitlementForeignRoleIs400(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	// A server in OUR tenant, but a role belonging to ANOTHER tenant.
	srvr, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "github", Transport: "http", EndpointOrCommand: "https://x", Status: "active"})
	other, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("role-other-%d", time.Now().UnixNano()))
	foreignRole, _ := st.CreateRole(ctx, other.ID, "intruder")

	rec := httptest.NewRecorder()
	srv.handleCreateEntitlement(rec, adminReq(ctx, http.MethodPost, "/v1/admin/entitlements",
		map[string]any{"roleId": foreignRole.ID, "mcpServerId": srvr.ID}, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("foreign-role entitlement = %d, want 400", rec.Code)
	}
	// Nothing should have been created, and no audit row written (validation precedes auditedTx).
	ents, _ := st.ListEntitlementsPage(ctx, tn.ID, nil, 0, false)
	if len(ents) != 0 {
		t.Fatalf("foreign-role entitlement must not be created: %+v", ents)
	}
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if len(evs) != 0 {
		t.Fatalf("no audit expected on rejected entitlement, got %+v", evs)
	}
}

func TestAdminDeleteEntitlementIsAuditedAnd204(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	srvr, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "github", Transport: "http", EndpointOrCommand: "https://x", Status: "active"})
	ent, _ := st.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: srvr.ID})

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodDelete, "/v1/admin/entitlements/"+ent.ID, nil, tn)
	req.SetPathValue("id", ent.ID)
	srv.handleDeleteEntitlement(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete entitlement = %d, want 204", rec.Code)
	}
	ents, _ := st.ListEntitlementsPage(ctx, tn.ID, nil, 0, false)
	if len(ents) != 0 {
		t.Fatalf("entitlement not deleted: %+v", ents)
	}
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if len(evs) != 1 || evs[0].Action != "entitlement.delete" || evs[0].Decision != "allow" {
		t.Fatalf("expected one entitlement.delete audit, got %+v", evs)
	}

	// Deleting a non-existent id is 404 (tenant-scoped ErrNotFound → fail()).
	rec2 := httptest.NewRecorder()
	req2 := adminReq(ctx, http.MethodDelete, "/v1/admin/entitlements/"+ent.ID, nil, tn)
	req2.SetPathValue("id", ent.ID)
	srv.handleDeleteEntitlement(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("double-delete = %d, want 404", rec2.Code)
	}
}

// TestAdminDeleteEntitlementMalformedIDIs404 proves a non-UUID {id} maps to
// 404, not a 500 leaking a raw Postgres invalid_text_representation error
// (audit B2b) — the sibling servers/artifacts handlers already had this
// coverage; DeleteEntitlement did not.
func TestAdminDeleteEntitlementMalformedIDIs404(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodDelete, "/v1/admin/entitlements/not-a-uuid", nil, tn)
	req.SetPathValue("id", "not-a-uuid")
	srv.handleDeleteEntitlement(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id delete = %d, want 404, body = %s", rec.Code, rec.Body)
	}
}

func TestAdminAuditQueryReturnsEvents(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	_, _ = st.AppendAuditEvent(ctx, store.AuditEvent{TenantID: tn.ID, Actor: "boss", Action: "server.create", Decision: "allow"})

	rec := httptest.NewRecorder()
	srv.handleListAudit(rec, adminReq(ctx, http.MethodGet, "/v1/admin/audit?limit=5", nil, tn))
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d", rec.Code)
	}
	var body struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Events) != 1 || body.Events[0]["action"] != "server.create" {
		t.Fatalf("audit events = %+v", body.Events)
	}
}

func TestAdminAuditPaginates(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	for i := 0; i < 3; i++ {
		_, _ = st.AppendAuditEvent(ctx, store.AuditEvent{TenantID: tn.ID, Actor: "a", Action: "x", Decision: "allow"})
	}
	rec := httptest.NewRecorder()
	srv.handleListAudit(rec, adminReq(ctx, http.MethodGet, "/v1/admin/audit?limit=2", nil, tn))
	if rec.Code != http.StatusOK {
		t.Fatalf("page1 = %d", rec.Code)
	}
	var p1 struct {
		Events     []map[string]any `json:"events"`
		Limit      int              `json:"limit"`
		NextCursor string           `json:"nextCursor"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p1)
	if len(p1.Events) != 2 || p1.Limit != 2 || p1.NextCursor == "" {
		t.Fatalf("page1 = %+v", p1)
	}
	rec2 := httptest.NewRecorder()
	srv.handleListAudit(rec2, adminReq(ctx, http.MethodGet, "/v1/admin/audit?limit=2&cursor="+p1.NextCursor, nil, tn))
	var p2 struct {
		Events     []map[string]any `json:"events"`
		NextCursor string           `json:"nextCursor"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &p2)
	if len(p2.Events) != 1 || p2.NextCursor != "" {
		t.Fatalf("page2 = %+v", p2)
	}
}

// TestAdminAuditRejectsBadCursor covers every malformed-cursor shape:
// not-base64 at all, and (audit B2a) valid base64 whose decoded "<nanos>:<id>"
// carries a syntactically well-formed but non-UUID id — decodeCursor validated
// only the shape, so "123:zzz" reached the uuid cast in store/audit.go and
// threw Postgres 22P02, surfacing as a 500 instead of the intended 400.
func TestAdminAuditRejectsBadCursor(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	cases := map[string]string{
		"not base64":                "not-base64!!",
		"valid base64, non-UUID id": base64.RawURLEncoding.EncodeToString([]byte("123:zzz")),
	}
	for name, cursor := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.handleListAudit(rec, adminReq(ctx, http.MethodGet, "/v1/admin/audit?cursor="+cursor, nil, tn))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("bad cursor (%s) = %d, want 400, body = %s", name, rec.Code, rec.Body)
			}
		})
	}
}

func TestAdminAuditQueryRejectsBadLimit(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)
	rec := httptest.NewRecorder()
	srv.handleListAudit(rec, adminReq(ctx, http.MethodGet, "/v1/admin/audit?limit=abc", nil, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit = %d, want 400", rec.Code)
	}
}

func TestAdminRouteRequiresAdminRole(t *testing.T) {
	// RequireRole denies a non-admin principal with 403 (fail-closed).
	h := authz.RequireRole("orbeat-admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/servers", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Subject: "u", Roles: []string{"orbeat-user"}}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", rec.Code)
	}
}

func TestAdminDeleteServerIsAuditedAnd204(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	m, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "del", Transport: "http", EndpointOrCommand: "https://y", Status: "active"})

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodDelete, "/v1/admin/servers/"+m.ID, nil, tn)
	req.SetPathValue("id", m.ID)
	srv.handleDeleteServer(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", rec.Code)
	}
	if _, err := st.GetMCPServer(ctx, tn.ID, m.ID); err == nil {
		t.Fatal("server not deleted")
	}
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if len(evs) != 1 || evs[0].Action != "server.delete" {
		t.Fatalf("expected server.delete audit, got %+v", evs)
	}
}

// roleDeleteRequest issues an authenticated DELETE /v1/admin/roles/{id}
// through the REAL router (srv.Handler().ServeHTTP): auth, the admin-role
// gate, and resolver middleware all run, unlike the adminReq/srv.handleXxx
// idiom used above in this file for the older server/entitlement/role tests.
// Driving the door production opens is deliberate here — a test that calls
// the handler directly cannot catch the route being registered under the
// wrong method (the pagination slice's C7 correction; see newPagingServer's
// doc comment in paging_test.go).
//
// It does NOT, by itself, catch the admin gate being dropped from this
// route: every call site below authenticates with an admin token, so a
// route silently downgraded to authed(...) would still return whatever the
// handler itself returns and this test would never notice. That gap is
// closed class-wide, not per-route, by
// TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite in admin_gate_test.go,
// which derives every admin(...) route from api.go's source and drives each
// one with a non-admin token — so a newly added admin route is covered
// automatically, without a hand-maintained list one route could still fall
// through.
func roleDeleteRequest(t *testing.T, srv *Server, tok, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/roles/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestAdminDeleteRoleCascadesAndAudits is the decisive test for §7 #10: a
// role with one server grant and one artifact grant is deleted, and the
// response, the cascade, and the audit record are all checked.
//
// The audit-metadata assertion is THE POINT of the feature (design doc §4) —
// a cascade that revokes grants while logging one line cannot answer "why
// did alice lose access?". It asserts the actual NAME STRINGS the store
// reported, not merely that the metadata keys are present: a Task 1 review
// found a mutant that made the store report UUIDs instead of names while
// every len()-only check stayed green (see
// TestDeleteRoleReportsWhatTheCascadeRevoked in internal/store/rbac_test.go).
// This test is also this slice's mandatory RED-PROOF target (Task 2 Step 7):
// dropping the handler's Metadata map must fail only the metadata assertions
// below, leaving the 200/counts/cascade assertions green.
func TestAdminDeleteRoleCascadesAndAudits(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	role, err := st.CreateRole(ctx, tn.ID, "del-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	srvr, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "del-srv", Transport: "http",
		EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if _, err := st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srvr.ID,
	}); err != nil {
		t.Fatalf("create entitlement: %v", err)
	}
	art, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "del-art", Visibility: "role",
		Content: "---\nname: x\ndescription: y\n---\nbody\n",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if _, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
		TenantID: tn.ID, RoleID: role.ID, ArtifactID: art.ID,
	}); err != nil {
		t.Fatalf("create artifact entitlement: %v", err)
	}

	rec := roleDeleteRequest(t, srv, tok, role.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete role = %d, want 200, body %s", rec.Code, rec.Body)
	}
	var body struct {
		EntitlementsRevoked         int `json:"entitlementsRevoked"`
		ArtifactEntitlementsRevoked int `json:"artifactEntitlementsRevoked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v, raw=%s", err, rec.Body)
	}
	if body.EntitlementsRevoked != 1 || body.ArtifactEntitlementsRevoked != 1 {
		t.Fatalf("body = %+v, want {entitlementsRevoked:1 artifactEntitlementsRevoked:1}", body)
	}

	// Both grants are actually gone.
	ents, err := st.ListEntitlementsPage(ctx, tn.ID, nil, 0, false)
	if err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	for _, e := range ents {
		if e.RoleID == role.ID {
			t.Errorf("entitlement %s survived the role deletion", e.ID)
		}
	}
	aents, err := st.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0, false)
	if err != nil {
		t.Fatalf("list artifact entitlements: %v", err)
	}
	for _, e := range aents {
		if e.RoleID == role.ID {
			t.Errorf("artifact entitlement %s survived the role deletion", e.ID)
		}
	}

	// Exactly one audit row, action/decision correct.
	evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("audit events = %d, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Action != "role.delete" || ev.Decision != "allow" {
		t.Fatalf("audit event = %+v, want action=role.delete decision=allow", ev)
	}

	// The metadata assertion IS the point: assert the actual strings, not
	// just presence.
	if ev.Metadata["name"] != "del-role" {
		t.Errorf("metadata[name] = %v, want %q", ev.Metadata["name"], "del-role")
	}
	if n, ok := ev.Metadata["entitlementsRevoked"].(float64); !ok || n != 1 {
		t.Errorf("metadata[entitlementsRevoked] = %v, want 1", ev.Metadata["entitlementsRevoked"])
	}
	if n, ok := ev.Metadata["artifactEntitlementsRevoked"].(float64); !ok || n != 1 {
		t.Errorf("metadata[artifactEntitlementsRevoked] = %v, want 1", ev.Metadata["artifactEntitlementsRevoked"])
	}
	servers, ok := ev.Metadata["servers"].([]any)
	if !ok || len(servers) != 1 || servers[0] != "del-srv" {
		t.Errorf("metadata[servers] = %v, want [del-srv]", ev.Metadata["servers"])
	}
	artifacts, ok := ev.Metadata["artifacts"].([]any)
	if !ok || len(artifacts) != 1 || artifacts[0] != "del-art" {
		t.Errorf("metadata[artifacts] = %v, want [del-art]", ev.Metadata["artifacts"])
	}
	// truncated must be present and false: only one server/artifact grant
	// existed, well under store.MaxGrantNames, so the name lists were not capped.
	// Red-proven: dropping the handler's "truncated" key from the Metadata
	// map (admin_roles.go) leaves every assertion above green — only this
	// one catches it.
	if trunc, ok := ev.Metadata["truncated"].(bool); !ok || trunc {
		t.Errorf("metadata[truncated] = %v, want false", ev.Metadata["truncated"])
	}
}

// This test does NOT separately force the audit-write step itself to fail
// and assert the delete rolls back (design doc §10). That mechanism is
// generic, not role-delete-specific: auditedTx (admin_audit_helper.go) folds
// the mutate error and the AppendAuditEvent error into the exact same
// `return e` inside store.InTx's closure, and InTx rolls back on ANY error
// the closure returns, full stop — proven directly for the store-level
// primitive by TestInTxRollsBackOnError (internal/store/store_tx_test.go),
// and proven for auditedTx's own composition (mutate error → rollback, zero
// audit rows, nothing dual-emitted) by TestAuditNoEmitOnRollback
// (internal/api/admin_audit_test.go). DeleteRole writes through the same
// tx-bound *Store as every other auditedTx caller, with no savepoint, no
// second connection, and no special-casing around the audit insert — so a
// third integration test forcing AppendAuditEvent to fail specifically on
// this route would just re-prove the same InTx guarantee those two already
// pin, not discriminate a role-delete-specific defect. Verified empirically
// during review (not just reasoned about): injecting a JSON-unmarshalable
// value into handleDeleteRole's audit metadata via go test -overlay forces
// AppendAuditEvent's json.Marshal to fail, and the role row + its
// entitlement survive with zero audit rows written — exactly what
// TestInTxRollsBackOnError + TestAuditNoEmitOnRollback already predict.

// TestAdminDeleteRoleNotFound covers all three shapes that must 404, mirroring
// store.TestDeleteRoleNotFound but through the real HTTP router. The
// cross-tenant case additionally proves the foreign role still exists
// afterwards (a delete that 404s must not have deleted anything).
func TestAdminDeleteRoleNotFound(t *testing.T) {
	ctx := context.Background()
	// tn (the resolved tenant for every request through srv) is intentionally
	// unused by name below — the whole point is that every request resolves
	// into it, distinct from other, and never into other by any path.
	srv, st, _, tok := newPagingServer(t)

	other, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("del-role-other-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	foreign, err := st.CreateRole(ctx, other.ID, "foreign-role")
	if err != nil {
		t.Fatalf("create foreign role: %v", err)
	}

	for _, tc := range []struct{ name, id string }{
		{"unknown uuid", "00000000-0000-0000-0000-000000000000"},
		{"malformed id", "not-a-uuid"},
		{"cross-tenant id", foreign.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := roleDeleteRequest(t, srv, tok, tc.id)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("delete role (%s) = %d, want 404, body %s", tc.name, rec.Code, rec.Body)
			}
		})
	}

	// The foreign role, targeted from the wrong tenant above, must still exist.
	roles, err := st.ListRolesPage(ctx, other.ID, nil, 0, false, "")
	if err != nil {
		t.Fatalf("list foreign-tenant roles: %v", err)
	}
	found := false
	for _, r := range roles {
		if r.ID == foreign.ID {
			found = true
		}
	}
	if !found {
		t.Error("foreign role no longer exists after a cross-tenant delete attempt")
	}
}
