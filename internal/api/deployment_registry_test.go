package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
)

// syncConfigBody drives handleSyncConfig with an injected resolved identity
// (the same shape TestHandleSyncConfigReturnsGatewayURL uses) and returns the
// decoded body. Both flags are read, not just the one under test, so a caller
// can assert the pair rather than one key in isolation.
func syncConfigBody(t *testing.T, srv *Server) struct {
	GatewayURL         string `json:"gateway_url"`
	DeploymentRegistry bool   `json:"deploymentRegistry"`
} {
	t.Helper()
	var body struct {
		GatewayURL         string `json:"gateway_url"`
		DeploymentRegistry bool   `json:"deploymentRegistry"`
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/config", nil)
	rc := authz.ResolvedContext{TenantID: "t1", UserID: "u1"}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/sync/config = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode sync config: %v", err)
	}
	return body
}

// probeReportRoute sends an UNAUTHENTICATED POST to /v1/sync/deployments
// through the real router and returns the status.
//
// 404 and 401 are the discriminator, and it is exact rather than approximate.
// An unregistered path never reaches auth, so the mux answers 404; a
// registered one is wrapped in authed(...), whose RequireAuth rejects a request
// with no bearer token as 401 before anything else runs. So 404 means the route
// DOES NOT EXIST and 401 means it does, with no third reading and no token
// needed. TestDeploymentRegistryOffByDefault below asserts the 404 and
// TestDeploymentRegistryOnRegistersTheRoute (sync_deployments.ee_test.go)
// asserts the 401 on the same probe: the pair is what proves this probe
// discriminates at all, rather than one of them passing because the probe
// always returns the same thing.
func probeReportRoute(t *testing.T, srv *Server) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sync/deployments",
		strings.NewReader(`{"installId":"00000000-0000-4000-8000-000000000001"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// TestDeploymentRegistryOffByDefault is gate G10 (docs/specs/2026-08-22-orbeat-
// artifact-deployment-registry-design.md sec 15): with nothing configured, the
// product default, orbeat records nothing about anybody's machines.
//
// BOTH halves are asserted, and each covers for the other's blind spot. A gate
// checking only the advertised flag passes on a server that registered the
// route anyway, and a gate checking only the 404 passes on a server telling
// every client the registry is on. The mutant this exists for is a
// default-on ship, whose cost is the one thing in this slice a later patch
// cannot recover: rows collected about named people who were never told.
//
// It is an ORDINARY test file, not a .ee_test.go one, deliberately. Both
// editions must answer exactly this way with nothing configured, so the same
// assertions run inside the generated Community tree on every `go test ./...`
// via TestGenerateProducesTestableTree. What Community additionally answers,
// that setting the knob still changes nothing, cannot live here (it is false
// in this build) and is proven by injection in internal/communitygen.
func TestDeploymentRegistryOffByDefault(t *testing.T) {
	// No SetDeploymentRegistry call at all: New's own default is the product
	// default, and this is what cmd/api produces with ORBEAT_DEPLOYMENT_REGISTRY
	// unset. Every dependency is nil because neither assertion touches one: an
	// unregistered route is answered by the mux, a registered one by
	// RequireAuth, and handleSyncConfig reads two fields. A store here would be
	// a moving part with nothing to move.
	srv := New(nil, nil, nil, nil, nil)

	if got := probeReportRoute(t, srv); got != http.StatusNotFound {
		t.Errorf("unauthenticated POST /v1/sync/deployments with the registry unconfigured = %d, want 404 "+
			"(401 would mean the collection route exists)", got)
	}
	if cfg := syncConfigBody(t, srv); cfg.DeploymentRegistry {
		t.Error("GET /v1/sync/config advertises deploymentRegistry: true with the registry unconfigured")
	}
}

// TestSetDeploymentRegistryOffStaysOff pins the setter's own arm: an explicit
// false is stored as false, so an operator who sets ORBEAT_DEPLOYMENT_REGISTRY
// to a falsey value gets the same answer as one who never set it. Without this
// the off-by-default gate above only covers the never-called path, and a setter
// that ignored its argument would look identical.
func TestSetDeploymentRegistryOffStaysOff(t *testing.T) {
	srv := New(nil, nil, nil, nil, nil)
	if srv.SetDeploymentRegistry(false) {
		t.Fatal("SetDeploymentRegistry(false) reported the registry as enabled")
	}
	if srv.deploymentRegistry {
		t.Fatal("SetDeploymentRegistry(false) stored true")
	}
}
