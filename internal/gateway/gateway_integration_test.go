package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/migrate"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

var gwDSN string

func TestMain(m *testing.M) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"), tcpostgres.WithUsername("orbeat"), tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-gateway-tests"),
		testcontainers.WithWaitStrategy(wait.ForAll(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second))))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dsn, _ := pg.ConnectionString(ctx, "sslmode=disable")
	db, _ := sql.Open("pgx", dsn)
	if err := migrate.Up(db); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = db.Close()
	gwDSN = dsn
	code := m.Run()
	_ = pg.Terminate(ctx)
	os.Exit(code)
}

func stubVerifier(tokenToPrincipal map[string]auth.Principal) mcpauth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		p, ok := tokenToPrincipal[token]
		if !ok {
			return nil, fmt.Errorf("%w: unknown token", mcpauth.ErrInvalidToken)
		}
		return principalToTokenInfo(p), nil
	}
}

func connectThroughGateway(t *testing.T, ctx context.Context, gatewayURL, token string) *mcp.ClientSession {
	t.Helper()
	parsed, err := neturl.Parse(gatewayURL)
	if err != nil {
		t.Fatalf("parse gateway url %q: %v", gatewayURL, err)
	}
	httpClient := &http.Client{Transport: bearerRoundTripper{token: token, allowedHost: parsed.Host, base: http.DefaultTransport}}
	transport := &mcp.StreamableClientTransport{Endpoint: gatewayURL, HTTPClient: httpClient}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect through gateway: %v", err)
	}
	return cs
}

func TestGatewayVerticalEntitledAndDenied(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	up := newUpstreamFixture(t)
	tn, _ := st.GetOrCreateTenantByName(ctx, "default")
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	srv, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "fixture", Transport: "http",
		EndpointOrCommand: up.URL, SecretRef: "", Status: "active",
	})
	_, _ = st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID, AllowedTools: []string{"echo"},
	})

	verifier := stubVerifier(map[string]auth.Principal{
		"alice-tok": {Subject: "kc-alice", Email: "alice@x.io", Roles: []string{"orbeat-user"}},
	})
	gw := New(st, authz.NewResolver(st, "default"), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	// Close gateway sessions (and their upstream MCP streams) before the upstream
	// fixture's httptest.Server.Close runs, so no active upstream connection
	// blocks teardown. Cleanups run LIFO; this is registered after the fixture's.
	t.Cleanup(gw.Close)

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "alice-tok")
	t.Cleanup(func() { _ = cs.Close() })

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "fixture__echo" {
		t.Fatalf("tools = %+v, want [fixture__echo]", tools.Tools)
	}

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__echo", Arguments: json.RawMessage(`{"text":"hi"}`)})
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if tc, ok := out.Content[0].(*mcp.TextContent); !ok || tc.Text != "hi" {
		t.Fatalf("echo result = %+v", out.Content)
	}

	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fixture__danger", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("expected denial for non-entitled tool")
	}

	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 50)
	var sawAllow, sawDeny bool
	for _, e := range evs {
		if e.Action == "gateway.tool.call" && e.Decision == "allow" {
			sawAllow = true
		}
		if e.Action == "gateway.tool.call" && e.Decision == "deny" {
			sawDeny = true
		}
	}
	if !sawAllow || !sawDeny {
		t.Fatalf("audit: allow=%v deny=%v; events=%+v", sawAllow, sawDeny, evs)
	}
}

func TestGatewayDegradesOnBadUpstreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	good := newUpstreamFixture(t)
	// Use a fresh tenant so audit/entitlement state doesn't collide with other tests.
	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("degrade-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")

	goodSrv, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "good", Transport: "http", EndpointOrCommand: good.URL, Status: "active"})
	// Dead HTTP upstream: connection-refused endpoint (port 1) → connectUpstream fails → skip+error.
	deadSrv, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "dead", Transport: "http", EndpointOrCommand: "http://127.0.0.1:1/mcp", Status: "active"})
	// stdio upstream: unsupported in P1-e → skipped BEFORE connecting + error audit.
	stdioSrv, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "local", Transport: "stdio", EndpointOrCommand: "some-cmd", Status: "active"})

	for _, s := range []store.MCPServer{goodSrv, deadSrv, stdioSrv} {
		if _, err := st.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: s.ID, AllowedTools: []string{"echo"}}); err != nil {
			t.Fatalf("entitle %s: %v", s.Name, err)
		}
	}

	verifier := stubVerifier(map[string]auth.Principal{
		"bob-tok": {Subject: "kc-bob-degrade", Email: "bob@x.io", Roles: []string{"orbeat-user"}},
	})
	gw := New(st, authz.NewResolver(st, tn.Name), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "bob-tok")
	t.Cleanup(func() { _ = cs.Close() })

	// Only the good upstream's tool is visible; dead + stdio are silently absent.
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "good__echo" {
		t.Fatalf("tools = %+v, want only [good__echo]", tools.Tools)
	}
	// The good tool still works through the degraded session.
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "good__echo", Arguments: json.RawMessage(`{"text":"still-here"}`)})
	if err != nil {
		t.Fatalf("CallTool good: %v", err)
	}
	if tc, ok := out.Content[0].(*mcp.TextContent); !ok || tc.Text != "still-here" {
		t.Fatalf("good result = %+v", out.Content)
	}

	// Audit: allow for good, error for dead + stdio.
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 100)
	decByTarget := map[string]string{}
	for _, e := range evs {
		if e.Action == "gateway.upstream.connect" {
			decByTarget[e.Target] = e.Decision
		}
	}
	if decByTarget[goodSrv.ID] != "allow" {
		t.Fatalf("good upstream decision = %q, want allow", decByTarget[goodSrv.ID])
	}
	if decByTarget[deadSrv.ID] != "error" {
		t.Fatalf("dead upstream decision = %q, want error", decByTarget[deadSrv.ID])
	}
	if decByTarget[stdioSrv.ID] != "error" {
		t.Fatalf("stdio upstream decision = %q, want error", decByTarget[stdioSrv.ID])
	}
}

func TestBuildSessionSkipsNonActiveServers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	// Both upstreams are reachable, so without the status guard the disabled one
	// would also connect/register — this proves the skip is by status, not reachability.
	active := newUpstreamFixture(t)
	disabled := newUpstreamFixture(t)

	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("status-skip-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")

	activeSrv, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "active-up", Transport: "http", EndpointOrCommand: active.URL, Status: "active"})
	disabledSrv, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "disabled-up", Transport: "http", EndpointOrCommand: disabled.URL, Status: "disabled"})

	for _, s := range []store.MCPServer{activeSrv, disabledSrv} {
		if _, err := st.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: s.ID, AllowedTools: []string{"echo"}}); err != nil {
			t.Fatalf("entitle %s: %v", s.Name, err)
		}
	}

	gw := New(st, authz.NewResolver(st, tn.Name), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)

	p := auth.Principal{Subject: "kc-status-skip", Email: "s@x.io", Roles: []string{"orbeat-user"}}
	sess, err := gw.buildSession(ctx, p)
	if err != nil {
		t.Fatalf("buildSession: %v", err)
	}
	t.Cleanup(sess.close)

	// The active upstream is brokered; the disabled (but entitled) one is not.
	brokered := map[string]bool{}
	for _, id := range sess.slugToServer {
		brokered[id] = true
	}
	if !brokered[activeSrv.ID] {
		t.Fatalf("active upstream must be brokered; slugToServer=%v", sess.slugToServer)
	}
	if brokered[disabledSrv.ID] {
		t.Fatalf("non-active upstream must NOT be brokered; slugToServer=%v", sess.slugToServer)
	}
	if len(sess.upstreams) != 1 {
		t.Fatalf("want exactly one connected upstream, got %d", len(sess.upstreams))
	}

	// A non-active skip is an intentional skip, not a connect failure: no
	// upstream-connect audit (allow OR error) should be written for it.
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 100)
	for _, e := range evs {
		if e.Action == "gateway.upstream.connect" && e.Target == disabledSrv.ID {
			t.Fatalf("non-active server must be skipped silently, but saw a %q connect audit for it", e.Decision)
		}
	}
}

// TestGatewaySSEUpstreamSurvivesRequestEnd proves the upstream SSE stream's
// lifetime is tied to the gateway session, not to the HTTP request that
// happened to trigger the session build (G1): after the first downstream
// connection ends, a second connection reusing the cached session must still
// reach the SSE upstream.
func TestGatewaySSEUpstreamSurvivesRequestEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	up := newSSEUpstreamFixture(t)
	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("sse-survive-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	srv, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "ssefix", Transport: "sse",
		EndpointOrCommand: up.URL, Status: "active",
	})
	_, _ = st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID, AllowedTools: []string{"echo"},
	})

	verifier := stubVerifier(map[string]auth.Principal{
		"carol-tok": {Subject: "kc-carol-sse", Email: "carol@x.io", Roles: []string{"orbeat-user"}},
	})
	gw := New(st, authz.NewResolver(st, tn.Name), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	// LIFO: close gateway sessions (upstream SSE streams) before the fixture's
	// httptest.Server.Close runs, so no active upstream connection blocks teardown.
	t.Cleanup(gw.Close)

	// Request 1: connect, verify the SSE upstream tool works, then end the
	// downstream session entirely.
	cs1 := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "carol-tok")
	t.Cleanup(func() { _ = cs1.Close() }) // safety on early failure; double-close is fine
	out, err := cs1.CallTool(ctx, &mcp.CallToolParams{Name: "ssefix__echo", Arguments: json.RawMessage(`{"text":"one"}`)})
	if err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
	if tc, ok := out.Content[0].(*mcp.TextContent); !ok || tc.Text != "one" {
		t.Fatalf("first echo result = %+v", out.Content)
	}
	if err := cs1.Close(); err != nil {
		t.Fatalf("close first downstream session: %v", err)
	}

	// Request 2: a fresh downstream connection with the same token hits the
	// cached gateway session; the SSE upstream must still be alive.
	cs2 := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "carol-tok")
	t.Cleanup(func() { _ = cs2.Close() })
	out, err = cs2.CallTool(ctx, &mcp.CallToolParams{Name: "ssefix__echo", Arguments: json.RawMessage(`{"text":"two"}`)})
	if err != nil {
		t.Fatalf("second CallTool through cached session: %v", err)
	}
	if tc, ok := out.Content[0].(*mcp.TextContent); !ok || tc.Text != "two" {
		t.Fatalf("second echo result = %+v", out.Content)
	}
}

// TestGatewayMaxAgeRebuildBoundsRevocation proves the session max-age ceiling
// (G4): entitlements are resolved at build time, so an active caller's session
// must be torn down and rebuilt once it exceeds sessionMaxAge — a revoked
// entitlement then takes effect even though the session was never idle.
func TestGatewayMaxAgeRebuildBoundsRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	up := newUpstreamFixture(t)
	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("maxage-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	srv, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "revoke", Transport: "http",
		EndpointOrCommand: up.URL, Status: "active",
	})
	ent, err := st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID, AllowedTools: []string{"echo"},
	})
	if err != nil {
		t.Fatalf("entitle: %v", err)
	}

	gw := New(st, authz.NewResolver(st, tn.Name), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)
	p := auth.Principal{Subject: "kc-eve-maxage", Email: "eve@x.io", Roles: []string{"orbeat-user"}}

	build := func() (*session, error) { return gw.buildSession(ctx, p) }
	sess1, err := gw.sessions.getOrBuild(p.Subject, time.Now(), build)
	if err != nil {
		t.Fatalf("first getOrBuild: %v", err)
	}
	if len(sess1.upstreams) != 1 {
		t.Fatalf("first session upstreams = %d, want 1", len(sess1.upstreams))
	}

	// Revoke: the cached session still holds the stale entitlement.
	if err := st.DeleteEntitlement(ctx, tn.ID, ent.ID); err != nil {
		t.Fatalf("delete entitlement: %v", err)
	}

	// Drive the cache past maxAge while the session stays "active" (recent
	// lastSeen is irrelevant — max-age is a ceiling, not an idle timer).
	sess2, err := gw.sessions.getOrBuild(p.Subject, time.Now().Add(sessionMaxAge+time.Second), build)
	if err != nil {
		t.Fatalf("second getOrBuild: %v", err)
	}
	if sess2 == sess1 {
		t.Fatal("session past maxAge must be rebuilt, got the cached one")
	}
	if len(sess2.upstreams) != 0 {
		t.Fatalf("rebuilt session upstreams = %d, want 0 (entitlement revoked)", len(sess2.upstreams))
	}

	// The rebuilt session denies the previously-entitled tool.
	stt, ctt := mcp.NewInMemoryTransports()
	if _, err := sess2.mcpServer.Connect(ctx, stt, nil); err != nil {
		t.Fatalf("connect to rebuilt session: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "unit-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ctt, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "revoke__echo", Arguments: json.RawMessage(`{"text":"x"}`)}); err == nil {
		t.Fatal("revoked tool call must be denied after the max-age rebuild")
	}

	// The evicted session's upstream connection was closed, not leaked.
	if err := sess1.upstreams[0].session.Ping(ctx, nil); !errors.Is(err, mcp.ErrConnectionClosed) {
		t.Fatalf("old session's upstream must be closed, Ping err = %v", err)
	}
}

// TestGatewayRebuildsAfterUpstreamDeath proves a dead upstream poisons the
// cached session only transiently (G5): the SDK keepalive + ErrConnectionClosed
// mark the session dirty, the cache evicts it, and the next downstream
// connection rebuilds against the still-listening upstream.
func TestGatewayRebuildsAfterUpstreamDeath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	up := newSSEUpstreamFixture(t)
	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("rebuild-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	srv, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "revive", Transport: "sse",
		EndpointOrCommand: up.URL, Status: "active",
	})
	_, _ = st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID, AllowedTools: []string{"echo"},
	})

	verifier := stubVerifier(map[string]auth.Principal{
		"dave-tok": {Subject: "kc-dave-rebuild", Email: "dave@x.io", Roles: []string{"orbeat-user"}},
	})
	gw := New(st, authz.NewResolver(st, tn.Name), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	gw.keepAlive = 100 * time.Millisecond // detect upstream death fast in-test
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	t.Cleanup(gw.Close)

	cs1 := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "dave-tok")
	t.Cleanup(func() { _ = cs1.Close() })
	if _, err := cs1.CallTool(ctx, &mcp.CallToolParams{Name: "revive__echo", Arguments: json.RawMessage(`{"text":"pre"}`)}); err != nil {
		t.Fatalf("CallTool before upstream death: %v", err)
	}

	// Sever every gateway→upstream connection; the fixture keeps listening.
	up.CloseClientConnections()

	// The cached session's upstream is now dead: calls through it must error
	// (and, once dirty-marking lands, flag the session for eviction).
	if _, err := cs1.CallTool(ctx, &mcp.CallToolParams{Name: "revive__echo", Arguments: json.RawMessage(`{"text":"dead"}`)}); err == nil {
		t.Fatal("CallTool against dead upstream unexpectedly succeeded")
	}

	// A fresh downstream connection must eventually get a rebuilt session that
	// reaches the revived (still-listening) upstream.
	gatewayHost, err := neturl.Parse(httpSrv.URL)
	if err != nil {
		t.Fatalf("parse gateway url %q: %v", httpSrv.URL, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok := func() bool {
			attemptCtx, cancelAttempt := context.WithTimeout(ctx, 3*time.Second)
			defer cancelAttempt()
			httpClient := &http.Client{Transport: bearerRoundTripper{token: "dave-tok", allowedHost: gatewayHost.Host, base: http.DefaultTransport}}
			client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, nil)
			cs, err := client.Connect(attemptCtx, &mcp.StreamableClientTransport{Endpoint: httpSrv.URL + "/mcp", HTTPClient: httpClient}, nil)
			if err != nil {
				return false
			}
			defer cs.Close()
			out, err := cs.CallTool(attemptCtx, &mcp.CallToolParams{Name: "revive__echo", Arguments: json.RawMessage(`{"text":"back"}`)})
			if err != nil {
				return false
			}
			tc, isText := out.Content[0].(*mcp.TextContent)
			return isText && tc.Text == "back"
		}()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway never rebuilt a working session after upstream death")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestGatewaySlowToolNotCappedByHeaderTimeout pins that slow tools work: the
// SDK's Streamable-HTTP server sends a tool result as the first SSE event,
// withholding response headers until then, so ANY header/deadline cap on the
// upstream HTTP client (e.g. Transport.ResponseHeaderTimeout) silently bounds
// every http-transport tool call. A ~2s tool must succeed through the gateway.
func TestGatewaySlowToolNotCappedByHeaderTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	up := newSlowUpstreamFixture(t, 2*time.Second)
	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("slowtool-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")
	srv, _ := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "slowfix", Transport: "http",
		EndpointOrCommand: up.URL, Status: "active",
	})
	_, _ = st.CreateEntitlement(ctx, store.Entitlement{
		TenantID: tn.ID, RoleID: role.ID, MCPServerID: srv.ID, AllowedTools: []string{"slow_echo"},
	})

	verifier := stubVerifier(map[string]auth.Principal{
		"frank-tok": {Subject: "kc-frank-slow", Email: "frank@x.io", Roles: []string{"orbeat-user"}},
	})
	gw := New(st, authz.NewResolver(st, tn.Name), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)
	t.Cleanup(gw.Close)

	cs := connectThroughGateway(t, ctx, httpSrv.URL+"/mcp", "frank-tok")
	t.Cleanup(func() { _ = cs.Close() })

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "slowfix__slow_echo", Arguments: json.RawMessage(`{"text":"patience"}`)})
	if err != nil {
		t.Fatalf("slow tool call must not be capped by an upstream client timeout: %v", err)
	}
	if tc, ok := out.Content[0].(*mcp.TextContent); !ok || tc.Text != "patience" {
		t.Fatalf("slow echo result = %+v", out.Content)
	}
}

// TestGatewaySlugCollisionFirstServerWins pins the defense against audit G3:
// Slugify is lossy, so two distinct catalog servers ("collide" and "collide!")
// can share a slug. Without a guard, the LAST server silently overwrites the
// slug→server map while the FIRST server's tools stay registered — so per-call
// RBAC authorizes server A's tools against server B's entitlements. Correct
// behavior: the first-registered server (stable store order: ORDER BY name)
// deterministically wins; the colliding later server is SKIPPED and audited
// gateway.upstream.connect/error; routing and authorization use the winner only.
func TestGatewaySlugCollisionFirstServerWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	// Server A ("collide", sorts first) serves echo + danger; server B
	// ("collide!", same slug) serves a distinct intruder tool.
	upA := newUpstreamFixtureWithTools(t, "echo", "danger")
	upB := newUpstreamFixtureWithTools(t, "intruder")

	tn, _ := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("slugcol-%d", time.Now().UnixNano()))
	role, _ := st.CreateRole(ctx, tn.ID, "orbeat-user")

	srvA, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "collide", Transport: "http", EndpointOrCommand: upA.URL, Status: "active"})
	srvB, _ := st.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "collide!", Transport: "http", EndpointOrCommand: upB.URL, Status: "active"})

	// The misroute setup from the audit: the caller is RESTRICTED on A (echo
	// only) but has a nil-AllowedTools (= every tool) entitlement on B. If the
	// slug resolves to B, A's tools are authorized against B's entitlement and
	// the restriction on A evaporates.
	if _, err := st.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: srvA.ID, AllowedTools: []string{"echo"}}); err != nil {
		t.Fatalf("entitle A: %v", err)
	}
	if _, err := st.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: srvB.ID, AllowedTools: nil}); err != nil {
		t.Fatalf("entitle B: %v", err)
	}

	gw := New(st, authz.NewResolver(st, tn.Name), nil, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)

	p := auth.Principal{Subject: "kc-slugcol", Email: "slug@x.io", Roles: []string{"orbeat-user"}}
	sess, err := gw.buildSession(ctx, p)
	if err != nil {
		t.Fatalf("buildSession: %v", err)
	}
	t.Cleanup(sess.close)

	// First-registered wins: the shared slug must resolve to A, and only ONE
	// upstream may be connected.
	if got := sess.slugToServer["collide"]; got != srvA.ID {
		t.Fatalf("slug 'collide' resolves to %q, want first server %q (B=%q)", got, srvA.ID, srvB.ID)
	}
	if len(sess.upstreams) != 1 {
		t.Fatalf("connected upstreams = %d, want 1 (colliding server must be skipped)", len(sess.upstreams))
	}

	// The skipped collider is audited as a connect error; the winner as allow.
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 100)
	decByTarget := map[string]string{}
	for _, e := range evs {
		if e.Action == "gateway.upstream.connect" {
			decByTarget[e.Target] = e.Decision
		}
	}
	if decByTarget[srvA.ID] != "allow" {
		t.Fatalf("winner upstream decision = %q, want allow", decByTarget[srvA.ID])
	}
	if decByTarget[srvB.ID] != "error" {
		t.Fatalf("colliding upstream decision = %q, want error", decByTarget[srvB.ID])
	}

	// Drive the session over in-memory transports: only A's tools exist.
	stt, ctt := mcp.NewInMemoryTransports()
	if _, err := sess.mcpServer.Connect(ctx, stt, nil); err != nil {
		t.Fatalf("connect session server: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "unit-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ctt, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	if !names["collide__echo"] || !names["collide__danger"] || names["collide__intruder"] {
		t.Fatalf("tools = %v, want A's tools only (no collide__intruder)", names)
	}

	// The entitled call routes to A and succeeds.
	out, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "collide__echo", Arguments: json.RawMessage(`{"text":"hi"}`)})
	if err != nil {
		t.Fatalf("CallTool entitled: %v", err)
	}
	if tc, ok := out.Content[0].(*mcp.TextContent); !ok || tc.Text != "hi" {
		t.Fatalf("echo result = %+v", out.Content)
	}

	// THE MISROUTE PIN: the caller is entitled to echo ONLY on the slug's
	// server. Today the slug resolves to B, whose nil-AllowedTools entitlement
	// authorizes ANY of A's registered tools — this call succeeds. Correct
	// behavior: authorized against A → denied.
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "collide__danger", Arguments: json.RawMessage(`{"text":"boom"}`)}); err == nil {
		t.Fatal("non-entitled tool on the winning server must be denied, not authorized against the collider's entitlement")
	}
}

// TestGatewayBuildFailureReturns503 pins audit G11: a server-side session-build
// failure (here: a dead store pool) must surface to MCP clients as
// 503 + Retry-After — a retryable server fault — NOT the SDK's 400 Bad Request,
// which clients like Claude Code treat as a permanent client error.
func TestGatewayBuildFailureReturns503(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	verifier := stubVerifier(map[string]auth.Principal{
		"greta-tok": {Subject: "kc-greta-503", Email: "greta@x.io", Roles: []string{"orbeat-user"}},
	})
	gw := New(st, authz.NewResolver(st, "default"), verifier, secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	t.Cleanup(gw.Close)
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)

	// Kill the store pool: every session build now fails server-side.
	st.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpSrv.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer greta-tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (a server fault must be retryable, not a client error)", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want %q", got, "5")
	}
}

func TestGatewayRejectsBadToken(t *testing.T) {
	ctx := context.Background()
	st, _ := store.New(ctx, gwDSN)
	t.Cleanup(st.Close)
	gw := New(st, authz.NewResolver(st, "default"), stubVerifier(map[string]auth.Principal{}), secrets.NewResolver(), "http://gw.test", "http://kc.test/realms/orbeat")
	httpSrv := httptest.NewServer(gw.Handler())
	t.Cleanup(httpSrv.Close)

	resp, err := http.Get(httpSrv.URL + "/mcp")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}
}
