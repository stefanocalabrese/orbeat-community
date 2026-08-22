package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareRejectsMissingBearer(t *testing.T) {
	ats := newAuthTestServer(t)
	v := newValidatorForTest(t, ats)

	called := false
	h := v.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/me", nil))

	if called {
		t.Fatal("handler must not run without a token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("expected WWW-Authenticate header on 401")
	}
}

func TestMiddlewareAcceptsValidBearerAndInjectsPrincipal(t *testing.T) {
	ats := newAuthTestServer(t)
	v := newValidatorForTest(t, ats)

	var gotSub string
	h := v.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := PrincipalFrom(r.Context()); ok {
			gotSub = p.Subject
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+ats.validToken(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotSub != "kc-sub-1" {
		t.Fatalf("principal subject = %q, want kc-sub-1", gotSub)
	}
}

var _ = context.Background
