package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// TestMiddlewareRefusesDeactivatedUserWith403 pins the STATUS, not merely the
// refusal. Resolve already refuses a deactivated user (TestResolveRefuses
// DeactivatedUser), but Middleware turns every non-seat-cap resolver error
// into 500, so before this branch existed a deactivated user got "internal
// error" on every ordinary route. Deprovisioning worked and looked like an
// outage: the operator who had just deactivated someone would reasonably
// conclude orbeat was broken rather than that it had obeyed.
//
// 403 and not 401: the token is valid, and re-authenticating changes nothing.
func TestMiddlewareRefusesDeactivatedUserWith403(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	r := NewResolver(s, "deact-mw-"+t.Name())
	p := auth.Principal{Subject: "deact-mw-1", Email: "d1@x.io"}
	rc, err := r.Resolve(ctx, p)
	if err != nil {
		t.Fatalf("seed the user: %v", err)
	}
	if err := s.DeactivateUser(ctx, rc.TenantID, rc.UserID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	called := false
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Error("the wrapped handler ran for a deactivated user, so deactivation does not bite on ordinary routes")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a deactivated user is a decision, not a server failure, and a 500 here reads as an orbeat outage to the operator who just deactivated them (body=%s)",
			rec.Code, rec.Body)
	}
}
