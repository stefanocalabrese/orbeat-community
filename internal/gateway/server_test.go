package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
)

// These two tests together are the gate for the gateway session lifecycle
// design's scope item 1/2 (2026-08-16 §6, §7 "In"): NewStreamableHTTPHandler
// must be called with non-nil options carrying a non-zero SessionTimeout, and
// that timeout must be sessionMaxAge specifically, not a value that could
// drift from it.
//
// The obvious implementation — parse server.go's own source with go/ast,
// following derivedAdminRoutes (admin_gate_test.go:39) — was tried first and
// abandoned after it produced an unexpected GREEN on both required mutants.
// Root cause, verified with a standalone repro before writing anything here:
// `go test -overlay` only substitutes what the Go toolchain COMPILES; a
// test's own os.ReadFile/parser.ParseFile of a source file at RUNTIME still
// reads the real file on disk, never the overlay's replacement. A test that
// re-parses server.go to check what Handler() passes is therefore
// structurally incapable of observing an overlay mutation to that same file
// — it would pass whether the call site is correct or not. (This is why
// derivedAdminRoutes itself is real-behavior-provable despite also reading
// source: the derived text there only builds the ROUTE LIST to iterate; the
// pass/fail signal comes from driving the REAL compiled router, which DOES
// reflect the overlay.)
//
// So instead: TestServerSessionTransportTimeoutFieldMatchesSessionMaxAge
// reads back a struct field New() actually set (a real compiled value, not
// re-parsed text), and TestHandlerActuallyReclaimsIdleTransportSessions
// drives Handler() over a real HTTP+MCP round trip and observes the SDK
// genuinely reclaim an idle session. Both are exercises of REAL compiled
// behavior, so both are overlay-provable — verified by the mutants in this
// slice's report.

// TestServerSessionTransportTimeoutFieldMatchesSessionMaxAge pins New()'s
// wiring: sessionTransportTimeout (Handler()'s source for
// mcp.StreamableHTTPOptions.SessionTimeout) must be exactly sessionMaxAge —
// the same constant our own session cache uses for its max-age eviction —
// not merely "some nonzero value" that could independently drift from it
// (design §3).
//
// It also pins THE OTHER HALF of that coupling, which two comments cited this
// test for while it did not actually assert it (found in review, 2026-08-29).
// sessionCache.tombstoneHorizon is computed from the CACHE's maxAge, not from
// the Server field above, and the A1 argument is that a tombstoned
// Mcp-Session-Id should not be forgotten while the SDK may still be holding
// the transport session behind it -- a DIAGNOSTIC bound since 2026-08-30, not
// a safety one: withSession refuses an id it holds no binding for, so
// forgetting a tombstone costs the 404's stated cause and nothing else.
// Changing New()'s newSessionCache call to
// newSessionCache(sessionTTL, sessionMaxAge/4, metrics) left the entire
// internal/gateway package green while quartering that horizon. The reap
// interval matters for the same argument (the sweep runs up to one ttl/2 tick
// late), so ttl is pinned here too.
func TestServerSessionTransportTimeoutFieldMatchesSessionMaxAge(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	srv := New(st, authz.NewResolver(st, "sessiontimeout-field-test"), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(srv.Close)

	if srv.sessionTransportTimeout != sessionMaxAge {
		t.Fatalf("sessionTransportTimeout = %v, want sessionMaxAge (%v)", srv.sessionTransportTimeout, sessionMaxAge)
	}
	if srv.sessions.maxAge != sessionMaxAge {
		t.Fatalf("sessions.maxAge = %v, want sessionMaxAge (%v): tombstoneHorizon reads THIS value, not sessionTransportTimeout, so how long a 404 can still name its cause depends on it", srv.sessions.maxAge, sessionMaxAge)
	}
	if srv.sessions.ttl != sessionTTL {
		t.Fatalf("sessions.ttl = %v, want sessionTTL (%v): the reap interval is ttl/2, and the sweep-runs-one-tick-late slack in tombstoneHorizon's argument is bounded by it", srv.sessions.ttl, sessionTTL)
	}
}

// TestHandlerActuallyReclaimsIdleTransportSessions drives a REAL MCP session
// end to end through Handler() and proves the SDK's own transport session
// (go-sdk@v1.7.0 mcp/streamable.go's h.sessions, keyed by MCP session id —
// distinct from our own per-principal sessionCache) is torn down once it
// sits idle past sessionTransportTimeout. This is the actual mechanism that
// closes design §2's leak: before this slice, idle SDK transport sessions
// were resident until process restart, unconditionally.
//
// A short, test-only timeout stands in for production's 5-minute
// sessionMaxAge (waiting out 5 real minutes is not practical in a unit
// test); TestServerSessionTransportTimeoutFieldMatchesSessionMaxAge pins
// that production actually uses sessionMaxAge, so together the two prove
// the full chain.
//
// Per design §6, this proves we asked the SDK to reclaim the idle transport
// session — a later request against the same session id is rejected. It
// does not, and cannot, prove the SDK's process memory actually shrinks;
// that map lives in a dependency this package cannot read.
func TestHandlerActuallyReclaimsIdleTransportSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("session-timeout-mech-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	verifier := stubVerifier(map[string]auth.Principal{
		"dana-tok": {Subject: "kc-dana-timeout", Email: "dana@x.io"},
	})

	const shortTimeout = 100 * time.Millisecond
	srv := &Server{
		store: st, resolver: authz.NewResolver(st, tn.Name), verifier: verifier,
		secrets:                 secrets.NewResolver(),
		resource:                "http://gw.test",
		authServer:              "http://kc.test/realms/orbeat",
		sessions:                newSessionCache(sessionTTL, sessionMaxAge, nil),
		keepAlive:               defaultUpstreamKeepAlive,
		sessionTransportTimeout: shortTimeout,
		logger:                  slog.Default(),
		metrics:                 telemetry.NewMetrics(otel.Meter("session-timeout-mech-test")),
	}
	t.Cleanup(srv.Close)

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "dana-tok")
	t.Cleanup(func() { _ = cs.Close() })

	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatalf("first ListTools (the SDK transport session must still be live): %v", err)
	}

	time.Sleep(shortTimeout + 400*time.Millisecond)

	_, err = cs.ListTools(ctx, nil)
	if err == nil {
		t.Fatal("second ListTools succeeded after the idle timeout elapsed: the SDK transport session was never reclaimed")
	}
	if !errors.Is(err, mcp.ErrSessionMissing) {
		t.Fatalf("second ListTools err = %v, want it to wrap mcp.ErrSessionMissing (go-sdk's §2.5.3 404-on-terminated-session signal)", err)
	}

	// AND PROVE THAT 404 CAME FROM THE SDK RATHER THAN FROM US. Since the A1
	// binding fix, withSession writes its OWN 404 for a transport session whose
	// gateway session was reclaimed, and the SDK client maps both 404s to
	// mcp.ErrSessionMissing -- so the assertion above, alone, would pass
	// whether or not the SDK ever reclaimed anything. (It genuinely does today:
	// this Server's cache carries the full sessionMaxAge while the test runs
	// for under a second, so no gateway session is ever evicted. The gate
	// closes the false green before it can open, it does not fix a live one.)
	//
	// The two are told apart by sessionRebuiltHeader: the gateway's 404 always
	// carries it, the SDK's own "session not found" never does. Replaying the
	// id by hand is the only vantage point -- the SDK client surfaces the
	// error, never the response headers.
	sid := cs.ID()
	if sid == "" {
		t.Fatal("client session reported no Mcp-Session-Id, so the replay below cannot address the reclaimed transport session")
	}
	replay, err := http.NewRequestWithContext(ctx, http.MethodPost, httpSrv.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("build replay request: %v", err)
	}
	replay.Header.Set("Authorization", "Bearer dana-tok")
	replay.Header.Set("Content-Type", "application/json")
	replay.Header.Set("Accept", "application/json, text/event-stream")
	replay.Header.Set(mcpSessionIDHeader, sid)
	resp, err := http.DefaultClient.Do(replay)
	if err != nil {
		t.Fatalf("replay the reclaimed Mcp-Session-Id: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("replayed Mcp-Session-Id got status %d, want 404", resp.StatusCode)
	}
	if marker := resp.Header.Get(sessionRebuiltHeader); marker != "" {
		t.Fatalf("replayed Mcp-Session-Id got a 404 carrying %s: %q -- that 404 was written by withSession's binding check, so this test proves nothing about the SDK reclaiming an idle transport session", sessionRebuiltHeader, marker)
	}
}
