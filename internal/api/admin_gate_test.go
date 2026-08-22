package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// adminGateHandleRe extracts (METHOD, path, handler) for every mux.Handle
// route registered through the admin(...) wrapper in api.go — the RBAC gate
// (RequireRole("orbeat-admin")) that must reject any authenticated non-admin
// principal before the resolver or the handler ever run. Deliberately
// restricted to the literal "admin(" wrapper, not "authed(" or any other:
// this is what makes the derived set below independent of a hand-maintained
// list (finding 1's actual defect — the existing roleDeleteRequest doc
// comment claimed route-driven coverage that didn't exist, and a
// hand-maintained list here would just be the same gap one level up).
var adminGateHandleRe = regexp.MustCompile(`mux\.Handle\("([A-Z]+) ([^"]+)",\s*admin\(s\.(\w+)\)\)`)

// derivedAdminRoutes reads the package's source and returns every {method,
// path} registered via admin(...), mapped to its handler name (for failure
// messages only). Not a hand-maintained list — see adminGateHandleRe's doc
// comment — so a newly added admin route is automatically covered by
// TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite below without anyone
// remembering to add a case, mirroring the derivedPaginatedRoutes /
// derivedGuardedRoutes idiom in openapi_test.go. Reads every non-test *.go
// file in the package (packageGoSource, openapi_test.go), not just api.go:
// the Enterprise admin routes are registered from routes_enterprise.go, and
// a single-file read would silently derive fewer routes than actually exist
// rather than failing loudly.
func derivedAdminRoutes(t *testing.T) map[string]string {
	t.Helper()
	src := packageGoSource(t)
	routes := map[string]string{}
	for _, m := range adminGateHandleRe.FindAllStringSubmatch(src, -1) {
		method, path, handler := m[1], m[2], m[3]
		routes[method+" "+path] = handler
	}
	if len(routes) == 0 {
		t.Fatal("no admin(...) routes found in package source — extraction regex is stale")
	}
	return routes
}

// TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite is the class-wide fix for
// finding 1 (spec+quality review of the role-deletion slice, commit
// 2c24164): roleDeleteRequest's doc comment claimed that driving the real
// router "cannot catch... the admin gate missing" — false, because every
// test in the package only ever sends an admin token, so an admin route
// silently downgraded to authed(...) (a copy-paste from the authed(...)
// block in api.go, or a bad merge) would fail NOTHING.
//
// Every route api.go registers through admin(...) is derived from source
// (derivedAdminRoutes, not an enumerated literal) and driven through the
// REAL router (srv.Handler().ServeHTTP) with a valid, authenticated
// non-admin bearer token. Each must 403. Path wildcards ({id}) are filled
// with a well-formed-but-unknown UUID so the request is safe to send for
// every method including DELETE: RequireRole runs before resolver.Middleware
// (audit B4 — see middleware_order_test.go), so a denied request must never
// reach a handler or write to the DB regardless of which route it targets.
// The trailing check (no user row for the probing subject) generalizes
// TestAdminRouteDeniesNonAdminBeforeAnyDBWrite's single-route assertion
// across every admin route in one pass, proving that ordering holds
// class-wide, not just for GET /v1/admin/servers.
//
// Red-proven (see Task-2-review-fixes commit): overlaying api.go to change
// admin(s.handleDeleteRole) -> authed(s.handleDeleteRole) fails this test's
// "DELETE /v1/admin/roles/{id}" subtest (and only that one); the same
// overlay applied to admin(s.handleCreateRole) -> authed(s.handleCreateRole)
// fails the "POST /v1/admin/roles" subtest instead — proving the coverage is
// real and derived, not accidentally specific to one route.
func TestAllAdminRoutesRejectNonAdminBeforeAnyDBWrite(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tenantName := fmt.Sprintf("admin-gate-%d", time.Now().UnixNano())
	idp := newMWOrderTestIdP(t)
	v, err := auth.NewValidator(ctx, auth.Config{Issuer: idp.srv.URL, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	srv := New(st, authz.NewResolver(st, tenantName), v, nil, nil)

	const subject = "kc-admin-gate-nonadmin"
	tok := idp.token(t, subject, []string{"orbeat-user"})

	routes := derivedAdminRoutes(t)
	var keys []string
	for r := range routes {
		keys = append(keys, r)
	}
	sort.Strings(keys)

	for _, route := range keys {
		route := route
		t.Run(route, func(t *testing.T) {
			parts := strings.SplitN(route, " ", 2)
			method, path := parts[0], parts[1]
			target := strings.ReplaceAll(path, "{id}", "00000000-0000-0000-0000-000000000000")

			req := httptest.NewRequest(method, target, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s (handler %s) with a non-admin token = %d, want 403, body=%s",
					route, routes[route], rec.Code, rec.Body)
			}
		})
	}

	// The gate must run before any DB write (audit B4): a flood of
	// unauthorized admin-route probes must never upsert a tenant/user row for
	// the denied subject, across EVERY admin route, not just one.
	tn, err := st.GetOrCreateTenantByName(ctx, tenantName)
	if err != nil {
		t.Fatalf("tenant lookup: %v", err)
	}
	if _, err := st.GetUserBySubject(ctx, tn.ID, subject); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no user row for the denied non-admin subject, got err=%v", err)
	}
}
