package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefanocalabrese/orbeat-community/internal/naming"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// upstreamConn is a live connection to one entitled upstream MCP server.
type upstreamConn struct {
	serverID string
	slug     string
	session  *mcp.ClientSession
	// cancel tears down the connection's context (severing e.g. the SSE hanging
	// GET). Set by buildSession once the upstream is live; nil-safe in close paths.
	cancel context.CancelFunc
	// transport is non-nil ONLY when this upstream owns its transport (it has a
	// custom CA). It MUST be CloseIdleConnections()'d on teardown: unlike the
	// shared dialGuardTransport this is per-session, so an unclosed one leaks a
	// connection pool per session build — directly underneath the session leak
	// v1.25.0 closed. nil for every upstream using the shared transport.
	transport *http.Transport
}

// bearerRoundTripper injects a static Authorization: Bearer header on every
// upstream HTTP request. token == "" means no header (public upstream).
//
// allowedHost gates the injection to requests targeting the entitled
// upstream's own host (audit finding G2). Go's http.Client strips sensitive
// headers — including Authorization — before following a cross-host redirect,
// but that stripping happens on the redirect *request* before it ever reaches
// the Transport. Our RoundTripper IS the Transport (see connectUpstream), so
// without this gate it would run on every hop of a redirect chain and
// unconditionally re-attach the secret Go had just stripped — handing the
// resolved upstream credential to whatever host a compromised or
// open-redirect upstream bounced the request to. allowedHost closes that path
// even for any hop that somehow reaches RoundTrip; CheckRedirect in
// connectUpstream is the primary control (redirects are refused outright), so
// in the ordinary case this gate is defense-in-depth, not the only line.
type bearerRoundTripper struct {
	token       string
	allowedHost string
	base        http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.token != "" && r.URL.Host == b.allowedHost {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(r)
}

// connectUpstream dials srv over its transport, injecting secret (already
// resolved) as a Bearer token, and returns a live session. Supports "http"
// (Streamable HTTP) and "sse" upstreams. Rejects an empty slug (a name that
// slugifies to "" would produce malformed "__tool" ids).
//
// caPEM is srv's already-resolved tls_ca_ref (empty means none: verify against
// the system pool via the shared dialGuardTransport, same as before this
// parameter existed). When non-empty, the returned upstreamConn.transport is
// non-nil and the caller MUST CloseIdleConnections() it on teardown — see
// upstreamTransport's doc comment (tlsconfig.go) for why a per-CA transport
// can't reuse the shared one.
//
// ctx must outlive the connection: the SDK's SSE client binds its hanging GET
// stream to it, so cancelling ctx severs the upstream session. keepAlive
// enables the SDK's ping loop, which auto-closes the session when the upstream
// stops responding.
func connectUpstream(ctx context.Context, srv store.MCPServer, secret string, caPEM string, keepAlive time.Duration) (*upstreamConn, error) {
	slug := naming.Slugify(srv.Name)
	if slug == "" {
		return nil, fmt.Errorf("gateway: server %q (%s) has an empty slug", srv.Name, srv.ID)
	}
	// Resolved up front (fail closed on a malformed endpoint) so
	// bearerRoundTripper can gate secret injection to this exact host — see
	// its doc comment for why that gate exists.
	endpointURL, err := url.Parse(srv.EndpointOrCommand)
	if err != nil {
		return nil, fmt.Errorf("gateway: parse upstream endpoint %q for %s: %w", srv.EndpointOrCommand, srv.Name, err)
	}
	// dialGuardTransport (dialguard.go) is a clone of http.DefaultTransport
	// (the global is never mutated) whose dialer additionally refuses cloud
	// metadata endpoints at dial time, after DNS resolution — closing the
	// SSRF vector write-time endpoint validation cannot see. Deliberately NO
	// ResponseHeaderTimeout: the SDK's Streamable-HTTP server answers tools/call
	// in SSE mode with headers withheld until the first event — the tool RESULT
	// — so a header timeout would cap every http-transport tool call. Build is
	// bounded by the dial watchdog; dead upstreams are detected by keepAlive.
	//
	// base defaults to the shared dialGuardTransport. When srv carries a
	// tls_ca_ref, caPEM is non-empty and base becomes a per-upstream transport
	// pinned to exactly that CA (upstreamTransport, tlsconfig.go) — a CLONE of
	// dialGuardTransport, so the SSRF guard above still applies to CA-pinned
	// upstreams too. owned tracks that per-upstream transport so the caller can
	// close it; nil when this upstream uses the shared transport.
	base := dialGuardTransport
	var owned *http.Transport
	if caPEM != "" {
		t, terr := upstreamTransport(caPEM)
		if terr != nil {
			return nil, fmt.Errorf("gateway: tls ca for %s: %w", srv.Name, terr)
		}
		base, owned = t, t
	}
	httpClient := &http.Client{
		Transport: bearerRoundTripper{token: secret, allowedHost: endpointURL.Host, base: base},
		// Redirecting upstreams are not a supported topology: refusing every
		// redirect (same-host or not) is the point, not a gap — it closes the
		// secret-exfiltration path at the source instead of relying solely on
		// the RoundTripper's host gate above. The SDK treats the returned 3xx
		// as a failed handshake/call, and the caller's existing skipUpstream
		// degrade path (server.go) takes over, same as any other connect error.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var transport mcp.Transport
	switch srv.Transport {
	case "http":
		transport = &mcp.StreamableClientTransport{Endpoint: srv.EndpointOrCommand, HTTPClient: httpClient}
	case "sse":
		transport = &mcp.SSEClientTransport{Endpoint: srv.EndpointOrCommand, HTTPClient: httpClient}
	default:
		return nil, fmt.Errorf("gateway: unsupported upstream transport %q", srv.Transport)
	}

	client := mcp.NewClient(gatewayImplementation(), &mcp.ClientOptions{KeepAlive: keepAlive})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("gateway: connect upstream %s: %w", srv.Name, err)
	}
	return &upstreamConn{serverID: srv.ID, slug: slug, session: session, transport: owned}, nil
}

// defaultedSchema returns s, or the permissive empty-object schema when s is nil.
// The SDK's Server.AddTool panics on a nil InputSchema; a non-SDK upstream may
// return a tool with no inputSchema, so we substitute {"type":"object"} (the
// SDK's documented "any input is valid" schema). We are the consumer of a remote
// tool and cannot know its schema better than the upstream — we just must not crash.
func defaultedSchema(s any) any {
	if s == nil {
		return map[string]any{"type": "object"}
	}
	return s
}

// registerProxies lists the upstream's tools and registers a namespaced
// passthrough proxy for each on gw. Returns the gateway-facing tool names.
// Arguments and results are forwarded verbatim (raw json), preserving
// forward-compat fields. markDirty is invoked when a proxied call hits a
// closed upstream connection (the SDK keepalive auto-closes dead sessions),
// flagging the owning gateway session for eviction + rebuild. tr instruments
// each proxied call with a span (fable-audit §7 #14) — nil is tolerated (no
// span, otherwise unchanged), matching this package's nil-safe metrics/limiter
// fields, so a Server built as a bare struct literal without New() (several
// existing tests) still works.
func registerProxies(ctx context.Context, gw *mcp.Server, conn *upstreamConn, markDirty func(), tr trace.Tracer) ([]string, error) {
	var names []string
	// Tools follows NextCursor across every page (audit G9): a single
	// ListTools call returns only page 1, silently dropping any tool beyond
	// the upstream's page size.
	for ut, err := range conn.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("gateway: list upstream tools (%s): %w", conn.slug, err)
		}
		proxyName := Namespace(conn.slug, ut.Name)
		gw.AddTool(
			&mcp.Tool{Name: proxyName, Description: ut.Description, InputSchema: defaultedSchema(ut.InputSchema)},
			func(callCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				// The upstream tools/call is the uninstrumented latency between the
				// gateway's own http.server span (otelhttp, Handler()) and pgx's
				// db.query spans — this closes that gap. Attributes are bounded-
				// cardinality catalog data only (server id/slug/tool name, fixed at
				// session-build time from the upstream's own tool list) — never the
				// per-principal subject, the same rule the rate-limiting slice
				// applied to its metrics: an unbounded label/attribute value would
				// make every span emitted for this instrument effectively
				// unbounded-cardinality telemetry.
				spanCtx := callCtx
				var span trace.Span
				if tr != nil {
					spanCtx, span = tr.Start(callCtx, "gateway.upstream.tool_call",
						trace.WithSpanKind(trace.SpanKindClient),
						trace.WithAttributes(
							attribute.String("gateway.upstream.server_id", conn.serverID),
							attribute.String("gateway.upstream.slug", conn.slug),
							attribute.String("gateway.tool.name", ut.Name),
						),
					)
					defer span.End()
				}
				res, err := conn.session.CallTool(spanCtx, &mcp.CallToolParams{
					Name:      ut.Name,
					Arguments: json.RawMessage(req.Params.Arguments),
				})
				if span != nil && err != nil {
					span.RecordError(err)
					span.SetStatus(codes.Error, err.Error())
				}
				if errors.Is(err, mcp.ErrConnectionClosed) {
					markDirty()
				}
				return res, err
			},
		)
		names = append(names, proxyName)
	}
	return names, nil
}
