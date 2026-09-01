package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
)

// TestDialUpstreamRefusesADisallowedEnvSecretRef is the gateway half of audit
// A4, and it is the half that matters for a row already in the database: a
// server carrying secretRef "env:ORBEAT_DB_URL" from before the allowlist
// existed must not dial, whatever its endpoint. Refusing the WRITE closes
// nothing for rows that are already stored.
//
// The variable is deliberately SET, and the endpoint is a live fixture that
// would accept the connection. Against an unset variable, or a dead endpoint,
// this test passes just as happily on a build with no allowlist at all: the
// dial fails for another reason and the assertion never discriminates. Here the
// only thing between the fixture and an Authorization header holding the
// Postgres password is the allowlist.
//
// It goes through s.secrets rather than calling the provider directly, because
// the claim under test is that the resolver the gateway HOLDS enforces the
// rule. cmd/gateway/main.go builds it with secrets.NewResolver(), the
// allowlisted constructor.
func TestDialUpstreamRefusesADisallowedEnvSecretRef(t *testing.T) {
	up := newUpstreamFixture(t)
	t.Setenv("ORBEAT_SECRET_ENV_ALLOW", "")
	t.Setenv("ORBEAT_DB_URL", "postgres://orbeat:hunter2@postgres:5432/orbeat")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s := &Server{secrets: secrets.NewResolver(), metrics: telemetry.NewMetrics(otel.Meter("a4-test"))}
	out := s.dialUpstream(ctx, store.MCPServer{
		ID: "srv-a4", Name: "Exfil", Transport: "http",
		EndpointOrCommand: up.URL, SecretRef: "env:ORBEAT_DB_URL", Status: "active",
	})
	// Registered BEFORE the assertions, and defensive about every field,
	// because the interesting run of this test is the FAILING one: with the
	// allowlist removed the dial succeeds and leaves a hanging GET open, and
	// newUpstreamFixture's own httptest Close then blocks on it. Measured: the
	// first draft took 138s to report the mutant instead of failing outright.
	// Cleanups run LIFO, so this one closes the session before the fixture
	// shuts down.
	t.Cleanup(func() { closeDial(out) })
	if out.reason != "secret resolve" {
		t.Fatalf("dialUpstream reason = %q (err %v), want \"secret resolve\": the upstream was dialled "+
			"with a credential the allowlist forbids", out.reason, out.err)
	}
	if out.conn != nil {
		t.Fatal("dialUpstream returned a connection for a refused secret ref")
	}
	if got := up.lastAuthHeader(); got != "" {
		t.Fatalf("upstream saw Authorization %q; the secret left the process", got)
	}
	if out.err == nil {
		t.Fatal("a skip must carry its cause")
	}
	if strings.Contains(out.err.Error(), "hunter2") {
		t.Fatalf("skip error carries the resolved value: %v", out.err)
	}
}

// The same server with an ALLOWED variable name dials cleanly and the token
// arrives at the upstream. Without this row the test above passes on a build
// that refuses every env: ref, which is a different bug wearing the same green.
func TestDialUpstreamAcceptsAnAllowlistedEnvSecretRef(t *testing.T) {
	up := newUpstreamFixture(t)
	t.Setenv("ORBEAT_SECRET_ENV_ALLOW", "")
	t.Setenv("ORBEAT_UPSTREAM_EXFIL_TOKEN", "tok-allowed")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s := &Server{secrets: secrets.NewResolver(), metrics: telemetry.NewMetrics(otel.Meter("a4-test"))}
	out := s.dialUpstream(ctx, store.MCPServer{
		ID: "srv-a4-ok", Name: "Exfil Ok", Transport: "http",
		EndpointOrCommand: up.URL, SecretRef: "env:ORBEAT_UPSTREAM_EXFIL_TOKEN", Status: "active",
	})
	t.Cleanup(func() { closeDial(out) })
	if out.reason != "" {
		t.Fatalf("dialUpstream skipped an allowlisted ref: reason=%q err=%v", out.reason, out.err)
	}
	if got := up.lastAuthHeader(); got != "Bearer tok-allowed" {
		t.Fatalf("upstream Authorization = %q, want \"Bearer tok-allowed\"", got)
	}
}

// closeDial releases whatever a dialOutcome actually holds, in any combination.
// dialUpstream returns four different shapes (refused before any dial, refused
// after, connected), and a cleanup that assumed one of them would panic on the
// others, turning a failed assertion into an unrelated crash.
func closeDial(out dialOutcome) {
	if out.watchdog != nil {
		out.watchdog.Stop()
	}
	if out.conn != nil {
		_ = out.conn.session.Close()
	}
	if out.cancel != nil {
		out.cancel()
	}
}
