package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
)

func handlerOK() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestRequireRoleAllows(t *testing.T) {
	h := RequireRole("orbeat-admin")(handlerOK())
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(auth.WithPrincipal(context.Background(), auth.Principal{Subject: "s", Roles: []string{"orbeat-user", "orbeat-admin"}}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRequireRoleForbidsMissingRole(t *testing.T) {
	called := false
	h := RequireRole("orbeat-admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(auth.WithPrincipal(context.Background(), auth.Principal{Subject: "s", Roles: []string{"orbeat-user"}}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if called {
		t.Fatal("handler must not run without the role")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRequireRoleForbidsNoPrincipal(t *testing.T) {
	h := RequireRole("orbeat-admin")(handlerOK())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when no principal", rec.Code)
	}
}
