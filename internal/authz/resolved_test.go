package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

func testCtx() context.Context { return context.Background() }

func TestMiddlewareInjectsResolvedContext(t *testing.T) {
	s, err := store.New(testCtx(), testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, _ := s.GetOrCreateTenantByName(testCtx(), "default")
	role, _ := s.CreateRole(testCtx(), tn.ID, "orbeat-user")

	r := NewResolver(s, "default")
	var seen ResolvedContext
	var ok bool
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen, ok = ResolvedFrom(req.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Subject: "kc-1", Email: "a@x.io", Roles: []string{"orbeat-user"}}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !ok {
		t.Fatal("ResolvedContext not injected")
	}
	if seen.TenantID != tn.ID || seen.UserID == "" {
		t.Fatalf("bad resolved ctx: %+v", seen)
	}
	if len(seen.RoleIDs) != 1 || seen.RoleIDs[0] != role.ID {
		t.Fatalf("RoleIDs = %v", seen.RoleIDs)
	}
}

func TestMiddlewareNoPrincipalIsUnauthorized(t *testing.T) {
	s, _ := store.New(testCtx(), testDSN)
	t.Cleanup(s.Close)
	r := NewResolver(s, "default")
	called := false
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if called {
		t.Fatal("handler must not run without a principal")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMiddlewareResolveErrorIs500(t *testing.T) {
	s, err := store.New(testCtx(), testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// Close the pool immediately so every DB call made by Resolve will fail.
	s.Close()

	r := NewResolver(s, "default")
	called := false
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { called = true }))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{
		Subject: "kc-broken", Email: "broken@x.io", Roles: []string{"orbeat-user"},
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if called {
		t.Fatal("wrapped handler must not be called on resolve error")
	}
	body := rec.Body.String()
	if strings.Contains(body, "default") {
		t.Fatalf("response body leaks internal detail (tenant name): %q", body)
	}
	if strings.Contains(body, "pgx") || strings.Contains(body, "sql") {
		t.Fatalf("response body leaks DB error detail: %q", body)
	}
}
