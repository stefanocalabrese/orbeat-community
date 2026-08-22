package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/goleak"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/ratelimit"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// swappableBearerRoundTripper lets a test change which bearer token attaches
// to subsequent requests over the SAME underlying transport (the same
// Mcp-Session-Id / jsonrpc2 connection). Used by
// TestRateLimitTwoPrincipalsOnOneConnectionDoNotStarveEachOther to prove the
// rate-limit key comes from the CURRENT call's req.GetExtra().TokenInfo, not
// from ctx: swapping the token mid-connection changes what GetExtra() sees on
// the NEXT POST without changing the jsonrpc2 connection's own frozen ctx
// (bound once, at whichever POST first created the connection).
type swappableBearerRoundTripper struct {
	mu          sync.Mutex
	token       string
	allowedHost string
	base        http.RoundTripper
}

func (b *swappableBearerRoundTripper) setToken(tok string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token = tok
}

func (b *swappableBearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	b.mu.Lock()
	tok := b.token
	b.mu.Unlock()
	if tok != "" && r.URL.Host == b.allowedHost {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return b.base.RoundTrip(r)
}

// ratelimitTestFixture sets up a tenant, an "orbeat-user" role, one upstream
// exposing an "echo" tool, and an entitlement granting that tool — the
// minimum shared state every test below needs before it can drive real
// tools/call and initialize traffic through the gateway's actual registered
// middleware chain (buildSession), which is the only way an overlay on
// server.go's registration can be observed by these tests.
// The two return values are deliberately distinct: authz.NewResolver takes
// the tenant NAME (a resolver constructor argument, matching every existing
// gateway test), while entitlement/audit store calls take the tenant ID
// (UUID) — the two are not interchangeable.
func ratelimitTestFixture(t *testing.T, name string) (st *store.Store, tenantID, tenantName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	up := newUpstreamFixture(t)
	tn, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	role, err := st.CreateRole(ctx, tn.ID, "orbeat-user")
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	srv, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "fixture", Transport: "http",
		EndpointOrCommand: up.URL, Status: "active",
	})
	if err != nil {
		t.Fatalf("mcp server: %v", err)
	}
	if _, err := st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID, AllowedTools: []string{"echo"},
	}); err != nil {
		t.Fatalf("entitlement: %v", err)
	}
	return st, tn.ID, tn.Name
}

// TestRateLimitThrottledToolCallErrorsWithoutAuditRow pins two properties of
// a throttled tools/call: (1) it surfaces to the caller as a real JSON-RPC
// error, never a panic or a silently-nil result, and (2) — the ordering pin —
// it writes NO gateway.tool.call audit row. If the limiter were registered
// AFTER rbacMiddleware (or in a separate AddReceivingMiddleware call that
// reverses the order), rbacMiddleware would write an "allow" row for a call
// that never actually executes.
func TestRateLimitThrottledToolCallErrorsWithoutAuditRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, tenantID, tenantName := ratelimitTestFixture(t, "ratelimit-audit")

	principal := auth.Principal{Subject: "kc-rl-audit", Email: "a@x.io", Roles: []string{"orbeat-user"}, ClientID: "orbeat-cli"}
	verifier := stubVerifier(map[string]auth.Principal{"alice-tok": principal})

	gw := New(st, authz.NewResolver(st, tenantName), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)
	// rps effectively zero: nothing reaccumulates within this test's real
	// wall-clock span (the plan's C1 lesson — the rejection must be
	// arithmetically forced, not incidental to timing).
	gw.SetLimiter(ratelimit.New(0.001, 1, 10*time.Minute, 100))
	gw.SetInitLimiter(ratelimit.New(0, 1, 10*time.Minute, 100)) // unlimited: this test is about the tools/call budget only

	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "alice-tok")
	t.Cleanup(func() { _ = cs.Close() })

	// Pre-drain the caller's sole token in-process (the established pattern:
	// see the plan's Task 4) so the ONLY tools/call in this test is the
	// throttled one — no "first call succeeds, second doesn't" indirection.
	key := ratelimit.KeyFor(principal)
	if ok, _ := gw.limiter.AllowAt(key, time.Now()); !ok {
		t.Fatal("predrain: expected the sole token to still be available")
	}

	_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"x"}`)})
	if err == nil {
		t.Fatal("expected the call to be throttled")
	}
	var jerr *jsonrpc.Error
	if !errors.As(err, &jerr) {
		t.Fatalf("expected a JSON-RPC error (not a panic or a swallowed nil), got %T: %v", err, err)
	}

	evs, err := st.ListAuditEventsByTenant(ctx, tenantID, 50)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	for _, e := range evs {
		if e.Action == "gateway.tool.call" {
			t.Fatalf("a throttled call must write NO gateway.tool.call audit row (ordering pin, spec §4.2); got: %+v", e)
		}
	}
}

// TestConcurrencyCapRejectedToolCallErrorsWithoutAuditRow is the concurrency
// cap's counterpart to TestRateLimitThrottledToolCallErrorsWithoutAuditRow:
// the SAME ordering pin (a rejected call writes NO gateway.tool.call audit
// row) applies to ratelimit.MCPConcurrency exactly as it does to
// ratelimit.MCP, because both must be registered ahead of rbacMiddleware in
// the ONE AddReceivingMiddleware call (spec §7). If the concurrency
// middleware were registered in a SEPARATE call, it would reverse effective
// order and rbacMiddleware would write an "allow" row for a call the cap
// never let through.
func TestConcurrencyCapRejectedToolCallErrorsWithoutAuditRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, tenantID, tenantName := ratelimitTestFixture(t, "concurrency-audit")

	principal := auth.Principal{Subject: "kc-cc-audit", Email: "c@x.io", Roles: []string{"orbeat-user"}, ClientID: "orbeat-cli"}
	verifier := stubVerifier(map[string]auth.Principal{"alice-tok": principal})

	gw := New(st, authz.NewResolver(st, tenantName), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)
	gw.SetLimiter(ratelimit.New(0, 1, 10*time.Minute, 100))     // unlimited: this test is about the concurrency cap only
	gw.SetInitLimiter(ratelimit.New(0, 1, 10*time.Minute, 100)) // unlimited
	gw.SetInflightCap(ratelimit.NewConcurrency(1, 10*time.Minute))

	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "alice-tok")
	t.Cleanup(func() { _ = cs.Close() })

	// Pre-occupy the sole in-flight slot in-process, mirroring the rate-limit
	// test's predrain: the ONLY tools/call this test drives over the wire is
	// the one that must be rejected by the cap, so there is no need for a
	// second, concurrent, blocking call just to trip it.
	key := ratelimit.KeyFor(principal)
	release, ok := gw.inflight.Acquire(key)
	if !ok {
		t.Fatal("predrain: expected the sole concurrency slot to still be available")
	}
	t.Cleanup(release)

	_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"x"}`)})
	if err == nil {
		t.Fatal("expected the call to be rejected by the concurrency cap")
	}
	var jerr *jsonrpc.Error
	if !errors.As(err, &jerr) {
		t.Fatalf("expected a JSON-RPC error (not a panic or a swallowed nil), got %T: %v", err, err)
	}

	evs, err := st.ListAuditEventsByTenant(ctx, tenantID, 50)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	for _, e := range evs {
		if e.Action == "gateway.tool.call" {
			t.Fatalf("a call rejected by the concurrency cap must write NO gateway.tool.call audit row (ordering pin, spec §7); got: %+v", e)
		}
	}
}

// TestRateLimitTwoPrincipalsOnOneConnectionDoNotStarveEachOther is the
// per-principal isolation assertion, and the vehicle for the ctx-vs-GetExtra
// red-proof: both principals share the SAME subject (hence the SAME cached
// gateway session — s.sessions is keyed by subject alone — and hence the
// SAME jsonrpc2 connection ctx, frozen at whichever call first established
// it) but different ClientID (azp), mirroring the real scenario spec §4.2
// names: one human's Claude Code and Codex sharing a subject. tool-a
// exhausts its own bucket; tool-b, a different rate-limit key, must still be
// served. This can only hold if the key comes from the CURRENT call's
// req.GetExtra().TokenInfo — a ctx-based key would collapse both onto
// whichever token happened to open the connection.
func TestRateLimitTwoPrincipalsOnOneConnectionDoNotStarveEachOther(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, _, tenantName := ratelimitTestFixture(t, "ratelimit-isolation")

	tokA := auth.Principal{Subject: "kc-rl-shared", Email: "s@x.io", Roles: []string{"orbeat-user"}, ClientID: "tool-a"}
	tokB := auth.Principal{Subject: "kc-rl-shared", Email: "s@x.io", Roles: []string{"orbeat-user"}, ClientID: "tool-b"}
	verifier := stubVerifier(map[string]auth.Principal{"shared-tok-a": tokA, "shared-tok-b": tokB})

	gw := New(st, authz.NewResolver(st, tenantName), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)
	gw.SetLimiter(ratelimit.New(0.001, 1, 10*time.Minute, 100)) // burst=1 per (subject,azp) key
	gw.SetInitLimiter(ratelimit.New(0, 1, 10*time.Minute, 100)) // unlimited

	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)

	parsed, err := url.Parse(httpSrv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	rt := &swappableBearerRoundTripper{token: "shared-tok-a", allowedHost: parsed.Host, base: http.DefaultTransport}
	httpClient := &http.Client{Transport: rt}
	transport := &mcp.StreamableClientTransport{Endpoint: httpSrv.URL + "/mcp", HTTPClient: httpClient}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, transport, nil) // initialize as tool-a
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	// tool-a's sole token, consumed.
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"a1"}`)}); err != nil {
		t.Fatalf("tool-a's first call (within its burst) should succeed: %v", err)
	}
	// tool-a is now throttled.
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"a2"}`)}); err == nil {
		t.Fatal("tool-a's second call should have been throttled")
	}

	// Swap to tool-b's token on the SAME connection (same Mcp-Session-Id,
	// same frozen jsonrpc2 connection ctx bound at tool-a's initialize).
	rt.setToken("shared-tok-b")
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"b1"}`)}); err != nil {
		t.Fatalf("tool-b should not be starved by tool-a's throttling: %v", err)
	}

	// "Share ONE Limiter": both keys are tracked in the same map.
	if got := gw.limiter.Len(); got != 2 {
		t.Errorf("limiter.Len() = %d, want 2 (kc-rl-shared|tool-a and kc-rl-shared|tool-b)", got)
	}
}

// TestRateLimitInitializeHasSeparateBudgetAndToolsListIsUnlimited pins spec
// §4.3: initialize is metered on its own (typically lower) budget, distinct
// from the tools/call budget, while tools/list — not in either middleware's
// method set — is never throttled regardless of how many times it's called.
func TestRateLimitInitializeHasSeparateBudgetAndToolsListIsUnlimited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, _, tenantName := ratelimitTestFixture(t, "ratelimit-init")

	principal := auth.Principal{Subject: "kc-rl-init", Email: "i@x.io", Roles: []string{"orbeat-user"}, ClientID: "orbeat-cli"}
	verifier := stubVerifier(map[string]auth.Principal{"init-tok": principal})

	gw := New(st, authz.NewResolver(st, tenantName), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)
	gw.SetLimiter(ratelimit.New(0, 1, 10*time.Minute, 100))         // unlimited tools/call: this test is about initialize/tools-list only
	gw.SetInitLimiter(ratelimit.New(0.001, 1, 10*time.Minute, 100)) // burst=1 initialize budget

	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)

	// First connection consumes the sole initialize token.
	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "init-tok")
	t.Cleanup(func() { _ = cs.Close() })

	// tools/list must never be throttled: it is in neither middleware's method set.
	for i := 0; i < 10; i++ {
		if _, err := cs.ListTools(ctx, nil); err != nil {
			t.Fatalf("tools/list call %d: unexpected error (tools/list must not be rate limited): %v", i, err)
		}
	}

	// A second, SEPARATE connection (fresh Mcp-Session-Id) from the SAME
	// (subject, azp) reuses the cached gateway session (subject-keyed) but
	// still performs its OWN initialize handshake, on the SAME now-drained
	// initialize budget.
	parsed, err := url.Parse(httpSrv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	httpClient2 := &http.Client{Transport: bearerRoundTripper{token: "init-tok", allowedHost: parsed.Host, base: http.DefaultTransport}}
	transport2 := &mcp.StreamableClientTransport{Endpoint: httpSrv.URL + "/mcp", HTTPClient: httpClient2}
	client2 := mcp.NewClient(&mcp.Implementation{Name: "e2e-client-2", Version: "0"}, nil)
	if cs2, err := client2.Connect(ctx, transport2, nil); err == nil {
		_ = cs2.Close()
		t.Fatal("second initialize on the same (subject, azp) budget should have been throttled")
	}
}

// TestServerCloseStopsEveryLimiterSweeperGoroutine is the goleak gate for
// Server.Close() (server.go): ratelimit.New and ratelimit.NewConcurrency each
// start a background sweeper goroutine (ratelimit.go's (*Limiter).run,
// concurrency.go's (*ConcurrencyLimiter).run) that only exits once Close()
// closes its stop channel. Close() nil-checks and closes all three fields —
// s.limiter, s.initLimiter, s.inflight — and drop any ONE of those calls and
// a sweeper leaks per Server, forever, with no other test in this package or
// internal/ratelimit catching it (measured against two of the three before
// this test existed).
//
// The obvious fixture — a bare New(...) followed by Close() — passes
// VACUOUSLY: New() does not construct any limiter; they are injected
// afterwards via SetLimiter/SetInitLimiter/SetInflightCap, exactly as
// cmd/gateway/main.go wires them (:157-158,170), and Close() nil-checks each
// one. A Server fresh from New() therefore holds no limiters to leak,
// regardless of what Close() does. This fixture calls all three setters with
// REAL limiters, mirroring main.go's shapes, so there is something for a
// dropped Close() call to actually leak.
//
// baseline is captured via goleak.IgnoreCurrent() AFTER store.New (this
// package's TestMain stands up a real Postgres; a bare goleak.VerifyNone
// would also report pgxpool/testcontainer goroutines that have nothing to do
// with this test's subject) but BEFORE New()/the limiters are constructed —
// so their sweepers are exactly what the diff between baseline and Close()
// can reveal.
//
// OBLIGATION: this test can only discover a leak in the setters it calls —
// because the limiters are injected rather than built inside New(), no
// runtime test can discover a setter nobody invokes. If Server ever grows a
// fourth limiter/setter pair, THIS TEST NEEDS A NEW LINE wiring it in, or its
// sweeper leaks with exactly zero coverage, the same way the three above did
// before this test existed.
func TestServerCloseStopsEveryLimiterSweeperGoroutine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	baseline := goleak.IgnoreCurrent()

	gw := New(st, authz.NewResolver(st, "limiter-leak-test"), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	// Real limiters, matching cmd/gateway/main.go's production shapes
	// (:157-158,170) — a nil limiter can never leak, so a Server without
	// these would make this test as vacuous as the bare-New() fixture above
	// warns against.
	gw.SetLimiter(ratelimit.New(20, 60, 10*time.Minute, 10000))
	gw.SetInitLimiter(ratelimit.New(1, 60, 10*time.Minute, 10000))
	gw.SetInflightCap(ratelimit.NewConcurrency(8, 10*time.Minute))

	gw.Close()

	goleak.VerifyNone(t, baseline)
}
