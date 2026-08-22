package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

func TestDefaultedSchema(t *testing.T) {
	if got := defaultedSchema(nil); got == nil {
		t.Fatal("nil schema must be defaulted to a non-nil object schema")
	}
	m, ok := defaultedSchema(nil).(map[string]any)
	if !ok || m["type"] != "object" {
		t.Fatalf("defaulted schema = %+v, want {type:object}", defaultedSchema(nil))
	}
	orig := map[string]any{"type": "object", "properties": map[string]any{}}
	if got := defaultedSchema(orig); got == nil {
		t.Fatal("non-nil schema must pass through")
	}
}

func TestConnectUpstreamInjectsBearerAndListsTools(t *testing.T) {
	up := newUpstreamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := store.MCPServer{
		ID: "srv-1", Name: "Up Stream", Transport: "http",
		EndpointOrCommand: up.URL, SecretRef: "", Status: "active",
	}
	conn, err := connectUpstream(ctx, srv, "tok-abc", "", 0)
	if err != nil {
		t.Fatalf("connectUpstream: %v", err)
	}
	t.Cleanup(func() { _ = conn.session.Close() })

	if conn.slug != "up-stream" {
		t.Fatalf("slug = %q, want up-stream", conn.slug)
	}
	tools, err := conn.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools.Tools)
	}
	if up.lastAuthHeader() != "Bearer tok-abc" {
		t.Fatalf("upstream Authorization = %q, want 'Bearer tok-abc'", up.lastAuthHeader())
	}
}

func TestConnectUpstreamRejectsEmptySlug(t *testing.T) {
	up := newUpstreamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A name that slugifies to "" must be rejected (would yield "__tool" ids).
	srv := store.MCPServer{ID: "srv-x", Name: "!!!", Transport: "http", EndpointOrCommand: up.URL, Status: "active"}
	if _, err := connectUpstream(ctx, srv, "", "", 0); err == nil {
		t.Fatal("want error for empty-slug server name")
	}
}

// TestConnectUpstreamWithoutCAUsesTheSharedTransport pins spec §6's blast-radius
// claim: a server with no tls_ca_ref must NOT get its own transport, so this
// change cannot regress connection pooling for upstreams that did not opt in.
func TestConnectUpstreamWithoutCAUsesTheSharedTransport(t *testing.T) {
	up := newUpstreamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := store.MCPServer{ID: "srv-1", Name: "Up Stream", Transport: "http", EndpointOrCommand: up.URL, Status: "active"}
	conn, err := connectUpstream(ctx, srv, "", "", 0)
	if err != nil {
		t.Fatalf("connectUpstream: %v", err)
	}
	t.Cleanup(func() { _ = conn.session.Close() })

	if conn.transport != nil {
		t.Fatal("an upstream without a CA must use the shared transport, not its own")
	}
}

// TestConnectUpstreamOwnsItsTransportWithCA is the converse: with a CA, the conn
// must carry a transport so teardown has something to close. Without this, a
// leak-free-looking implementation that simply never sets the field would pass
// the test above and leak nothing only because it also pins nothing.
func TestConnectUpstreamOwnsItsTransportWithCA(t *testing.T) {
	up := newUpstreamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := store.MCPServer{ID: "srv-1", Name: "Up Stream", Transport: "http", EndpointOrCommand: up.URL, Status: "active"}
	conn, err := connectUpstream(ctx, srv, "", testCAPEM, 0)
	if err != nil {
		t.Fatalf("connectUpstream: %v", err)
	}
	t.Cleanup(func() { _ = conn.session.Close() })

	if conn.transport == nil {
		t.Fatal("a CA-configured upstream must own its transport so teardown can close it")
	}
}

// TestConnectUpstreamBadCASkipsTheUpstream proves an unparseable PEM FAILS the
// dial rather than silently falling back to the system pool. A fallback would
// connect to an upstream the operator explicitly asked to pin, with nothing
// user-visible looking wrong.
func TestConnectUpstreamBadCASkipsTheUpstream(t *testing.T) {
	_, err := connectUpstream(context.Background(), store.MCPServer{
		ID: "srv-1", Name: "x", Transport: "http", EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	}, "", "not a pem", 0)
	if !errors.Is(err, errBadCAPEM) {
		t.Fatalf("err = %v, want errBadCAPEM", err)
	}
}

func TestRegisterProxiesForwardsCall(t *testing.T) {
	up := newUpstreamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := store.MCPServer{ID: "srv-1", Name: "up", Transport: "http", EndpointOrCommand: up.URL, Status: "active"}
	conn, err := connectUpstream(ctx, srv, "", "", 0)
	if err != nil {
		t.Fatalf("connectUpstream: %v", err)
	}
	t.Cleanup(func() { _ = conn.session.Close() })

	gw := mcp.NewServer(&mcp.Implementation{Name: "orbeat-gateway", Version: "test"}, nil)
	// nil tracer: this test is about call forwarding, not tracing.
	names, err := registerProxies(ctx, gw, conn, func() {}, nil)
	if err != nil {
		t.Fatalf("registerProxies: %v", err)
	}
	if len(names) != 1 || names[0] != "up__echo" {
		t.Fatalf("registered = %v, want [up__echo]", names)
	}

	st, ct := mcp.NewInMemoryTransports()
	if _, err := gw.Connect(ctx, st, nil); err != nil {
		t.Fatalf("gw.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	out, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "up__echo",
		Arguments: json.RawMessage(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(out.Content) == 0 {
		t.Fatalf("empty content")
	}
	if tc, ok := out.Content[0].(*mcp.TextContent); !ok || tc.Text != "hello" {
		t.Fatalf("proxied result = %+v, want text 'hello'", out.Content)
	}
}

// TestRegisterProxiesEmitsSpanForCall is the fable-audit §7 #14 gate for the
// proxy-hop span: the actual upstream tools/call is the uninstrumented
// latency between the gateway's own http.server span (otelhttp) and pgx's
// db.query spans, and this pins that registerProxies' closure opens (and
// ends) a span around exactly that call, carrying only bounded-cardinality
// catalog attributes (server id, slug, tool name) — never the per-principal
// subject/caller identity, matching the rule the rate-limiting slice applied
// to its metrics.
//
// A real sdktrace.TracerProvider + tracetest.SpanRecorder is injected
// directly (not a global otel.SetTracerProvider mutation), mirroring
// internal/telemetry's own queryTracer{tr: tp.Tracer(...)} test pattern —
// deterministic under -race/parallel test execution, unlike global state.
func TestRegisterProxiesEmitsSpanForCall(t *testing.T) {
	up := newUpstreamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := store.MCPServer{ID: "srv-span", Name: "spanup", Transport: "http", EndpointOrCommand: up.URL, Status: "active"}
	conn, err := connectUpstream(ctx, srv, "", "", 0)
	if err != nil {
		t.Fatalf("connectUpstream: %v", err)
	}
	t.Cleanup(func() { _ = conn.session.Close() })

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	gw := mcp.NewServer(&mcp.Implementation{Name: "orbeat-gateway", Version: "test"}, nil)
	if _, err := registerProxies(ctx, gw, conn, func() {}, tp.Tracer("gateway-test")); err != nil {
		t.Fatalf("registerProxies: %v", err)
	}

	st, ct := mcp.NewInMemoryTransports()
	if _, err := gw.Connect(ctx, st, nil); err != nil {
		t.Fatalf("gw.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "spanup__echo",
		Arguments: json.RawMessage(`{"text":"hi"}`),
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	ended := rec.Ended()
	idx := -1
	for i, s := range ended {
		if s.Name() == "gateway.upstream.tool_call" {
			idx = i
			break
		}
	}
	if idx == -1 {
		names := make([]string, len(ended))
		for i, s := range ended {
			names[i] = s.Name()
		}
		t.Fatalf("no span named gateway.upstream.tool_call recorded; got: %v", names)
	}
	span := ended[idx]
	if span.EndTime().Before(span.StartTime()) {
		t.Fatalf("span end (%v) before start (%v)", span.EndTime(), span.StartTime())
	}
	attrs := map[string]string{}
	for _, a := range span.Attributes() {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs["gateway.upstream.server_id"] != "srv-span" {
		t.Errorf("gateway.upstream.server_id = %q, want %q", attrs["gateway.upstream.server_id"], "srv-span")
	}
	if attrs["gateway.upstream.slug"] != "spanup" {
		t.Errorf("gateway.upstream.slug = %q, want %q", attrs["gateway.upstream.slug"], "spanup")
	}
	if attrs["gateway.tool.name"] != "echo" {
		t.Errorf("gateway.tool.name = %q, want %q", attrs["gateway.tool.name"], "echo")
	}
	if _, hasSubject := attrs["subject"]; hasSubject {
		t.Errorf("span must never carry a per-principal subject attribute, got one: %+v", attrs)
	}
}

// TestRegisterProxiesFollowsToolPagination pins audit G9: an upstream serving
// more tools than one list page must have ALL of them proxied, not just page 1.
// The SDK server paginates at mcp.DefaultPageSize (1000) by default; the
// fixture sets PageSize small so a multi-page list is cheap — the client-side
// cursor-following under test is identical regardless of the server's page size.
func TestRegisterProxiesFollowsToolPagination(t *testing.T) {
	const pageSize = 3
	toolNames := []string{"t1", "t2", "t3", "t4", "t5"} // pageSize+2 → two pages
	up := newUpstreamFixtureWithOptions(t, &mcp.ServerOptions{PageSize: pageSize}, toolNames...)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := store.MCPServer{ID: "srv-pag", Name: "pag", Transport: "http", EndpointOrCommand: up.URL, Status: "active"}
	conn, err := connectUpstream(ctx, srv, "", "", 0)
	if err != nil {
		t.Fatalf("connectUpstream: %v", err)
	}
	t.Cleanup(func() { _ = conn.session.Close() })

	// Sanity: the fixture really paginates — page 1 alone is not the full list.
	page1, err := conn.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools page 1: %v", err)
	}
	if len(page1.Tools) != pageSize || page1.NextCursor == "" {
		t.Fatalf("fixture must paginate: page1 has %d tools (cursor %q), want %d + a cursor", len(page1.Tools), page1.NextCursor, pageSize)
	}

	gw := mcp.NewServer(&mcp.Implementation{Name: "orbeat-gateway", Version: "test"}, nil)
	// nil tracer: this test is about pagination, not tracing.
	names, err := registerProxies(ctx, gw, conn, func() {}, nil)
	if err != nil {
		t.Fatalf("registerProxies: %v", err)
	}
	if len(names) != len(toolNames) {
		t.Fatalf("registered %d tools %v, want all %d (tools beyond page 1 silently dropped)", len(names), names, len(toolNames))
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, tn := range toolNames {
		if !got["pag__"+tn] {
			t.Fatalf("tool pag__%s missing from registered set %v", tn, names)
		}
	}
}

// headerSink is a plain (non-MCP) HTTP server that records the Authorization
// header of every request it receives. Used as a redirect *target* — it never
// needs to speak MCP, since these tests only care about whether the bearer
// secret reached it, not about completing a handshake through it.
type headerSink struct {
	*httptest.Server
	mu   sync.Mutex
	auth string
	hits int
}

func newHeaderSink(t *testing.T) *headerSink {
	t.Helper()
	sink := &headerSink{}
	sink.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink.mu.Lock()
		sink.auth = r.Header.Get("Authorization")
		sink.hits++
		sink.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	return sink
}

func (s *headerSink) received() (auth string, hits int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth, s.hits
}

// TestConnectUpstreamNeverForwardsBearerAcrossRedirectHost pins audit finding
// G2: a redirecting upstream must never receive the resolved bearer secret
// re-attached to a hop that leaves the entitled upstream's host. bearerRoundTripper
// IS the http.Client's Transport, so — unlike a normal caller — it runs on
// EVERY hop of a redirect chain, including ones Go's Client would otherwise
// have stripped the Authorization header from for being cross-host.
func TestConnectUpstreamNeverForwardsBearerAcrossRedirectHost(t *testing.T) {
	b := newHeaderSink(t) // the attacker-controlled redirect target

	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(a.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv := store.MCPServer{
		ID: "srv-redirect", Name: "Redirecting Upstream", Transport: "http",
		EndpointOrCommand: a.URL, Status: "active",
	}
	// A redirecting upstream can never complete a real MCP handshake (b speaks
	// no MCP protocol), so a connect error is expected either way — what this
	// test pins is whether the secret crossed hosts before that failure.
	conn, _ := connectUpstream(ctx, srv, "tok-secret", "", 0)
	if conn != nil {
		t.Cleanup(func() { _ = conn.session.Close() })
	}

	if auth, _ := b.received(); auth != "" {
		t.Fatalf("cross-host redirect target received Authorization = %q, want empty — secret must not cross hosts", auth)
	}
}

// TestConnectUpstreamRefusesRedirectChainToOtherHost pins that a two-hop
// redirect chain (A bounces same-host to A/next, which then bounces
// cross-host to B) never reaches its ultimate cross-host target B — the
// pattern an attacker-controlled open redirect on a legitimate upstream would
// typically use. It does NOT, by itself, pin the same-host leg (A -> A/next)
// in isolation: A carries no request counter of its own here, only the
// cross-host sink b does, so a CheckRedirect that followed same-host hops and
// refused only cross-host ones would leave THIS test passing unchanged (B is
// still never reached, just one hop later than a direct A -> B redirect
// would produce). See TestConnectUpstreamRefusesSameHostRedirect for the
// same-host leg pinned on its own.
func TestConnectUpstreamRefusesRedirectChainToOtherHost(t *testing.T) {
	b := newHeaderSink(t)

	var a *httptest.Server
	a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			http.Redirect(w, r, b.URL, http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, a.URL+"/next", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(a.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv := store.MCPServer{
		ID: "srv-redirect-chain", Name: "Redirecting Upstream Chain", Transport: "http",
		EndpointOrCommand: a.URL, Status: "active",
	}
	conn, err := connectUpstream(ctx, srv, "tok-secret", "", 0)
	if conn != nil {
		t.Cleanup(func() { _ = conn.session.Close() })
	}
	if err == nil {
		t.Fatal("want connect failure for a redirecting upstream, got nil error (redirects must not be followed)")
	}
	if auth, hits := b.received(); auth != "" || hits != 0 {
		t.Fatalf("redirect chain's ultimate target received a request (hits=%d, auth=%q), want it never contacted", hits, auth)
	}
}

// TestConnectUpstreamRefusesSameHostRedirect pins the same-host leg that
// TestConnectUpstreamRefusesRedirectChainToOtherHost's comment used to claim
// (falsely) it already covered: A redirects to A/next, still on A's own
// host, with no cross-host hop involved at all. CheckRedirect in
// connectUpstream refuses every redirect unconditionally, so /next must never
// be requested. A narrowed CheckRedirect that followed same-host hops and
// refused only cross-host ones would still fail this connect (a.URL/next
// speaks no MCP protocol either), so the assertion that matters is the hit
// count on /next, not merely that connectUpstream returned an error.
func TestConnectUpstreamRefusesSameHostRedirect(t *testing.T) {
	var mu sync.Mutex
	nextHits := 0

	var a *httptest.Server
	a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			mu.Lock()
			nextHits++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, a.URL+"/next", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(a.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv := store.MCPServer{
		ID: "srv-same-host-redirect", Name: "Same Host Redirect", Transport: "http",
		EndpointOrCommand: a.URL, Status: "active",
	}
	conn, err := connectUpstream(ctx, srv, "", "", 0)
	if conn != nil {
		t.Cleanup(func() { _ = conn.session.Close() })
	}
	if err == nil {
		t.Fatal("want connect failure for a redirecting upstream, got nil error (redirects must not be followed)")
	}

	mu.Lock()
	hits := nextHits
	mu.Unlock()
	if hits != 0 {
		t.Fatalf("same-host redirect target /next received %d requests, want 0 — the same-host hop must be refused too, not just cross-host", hits)
	}
}

// TestBearerRoundTripperHostGate pins the defense-in-depth layer on its own:
// the redirect-driven tests above go green on CheckRedirect alone (the target
// is never contacted), so without this direct RoundTrip test the host gate
// could silently regress while every other test stays green.
func TestBearerRoundTripperHostGate(t *testing.T) {
	sink := newHeaderSink(t)
	sinkHost := strings.TrimPrefix(sink.URL, "http://")

	cases := []struct {
		name     string
		host     string
		wantAuth string
	}{
		{name: "matching host injects", host: sinkHost, wantAuth: "Bearer tok-secret"},
		{name: "cross-host request stays bare", host: "other.example:9999", wantAuth: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := bearerRoundTripper{token: "tok-secret", allowedHost: tc.host, base: http.DefaultTransport}
			req, err := http.NewRequest(http.MethodGet, sink.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			resp.Body.Close()
			if auth, _ := sink.received(); auth != tc.wantAuth {
				t.Fatalf("sink received Authorization %q, want %q", auth, tc.wantAuth)
			}
		})
	}
}
