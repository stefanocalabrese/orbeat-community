package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/govern"
	"github.com/stefanocalabrese/orbeat-community/internal/httpx"
	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/naming"
	"github.com/stefanocalabrese/orbeat-community/internal/ratelimit"
	"github.com/stefanocalabrese/orbeat-community/internal/rbac"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
	"github.com/stefanocalabrese/orbeat-community/internal/version"
)

const (
	// sessionMaxAge is a hard ceiling on session age regardless of activity:
	// entitlements/roles are resolved at build time, so a rebuild at most this
	// old bounds revocation staleness. Also reused, unchanged, as the SDK
	// transport's own SessionTimeout below -- see Handler().
	sessionMaxAge = 5 * time.Minute
	// sessionTTL evicts a cached session not used for this long (idle
	// ceiling). Expressed as a multiple of sessionMaxAge rather than a
	// standalone literal, and deliberately kept > sessionMaxAge: expired()
	// (session.go) ORs an idle-past-ttl check with an age-past-maxAge check,
	// and a session's lastSeen is never before its builtAt, so the idle check
	// can only be the one that fires when ttl < maxAge. With ttl > maxAge
	// (today, and for any future maxAge under this formula) the idle rule is
	// provably inert in production -- max-age is the tighter, safety-relevant
	// bound, and sessionCache's idle-ttl mechanism exists as its
	// general-purpose second eviction rule, exercised directly with a
	// tighter ttl by session_test.go. Deriving sessionTTL from sessionMaxAge
	// means raising sessionMaxAge can never silently shrink that margin and
	// activate this second, unintended rule (gateway session lifecycle
	// design, 2026-08-16 §3 -- verified: the resulting 10m is numerically
	// unchanged from the prior standalone literal).
	sessionTTL = 2 * sessionMaxAge
	// upstreamDialTimeout bounds connect+tool-listing per upstream during a
	// session build. Enforced by a cancel-on-failure watchdog, NOT
	// context.WithTimeout: the connect ctx must outlive the dial because the
	// SDK's SSE client binds the hanging GET stream to it.
	upstreamDialTimeout = 10 * time.Second
	// sessionBuildDBTimeout bounds the DB-resolution phase of a session build
	// (the build ctx is cancel- and deadline-free, so without this a hung pool
	// would wedge the singleflight and every same-subject caller forever).
	sessionBuildDBTimeout = 10 * time.Second
	// defaultUpstreamKeepAlive is the SDK ping interval for upstream sessions;
	// a failed ping auto-closes the session so calls fail fast instead of
	// hitting a silently dead upstream.
	defaultUpstreamKeepAlive = 30 * time.Second
)

// gatewayImplementation returns the MCP Implementation both this gateway's
// server-facing leg (mcp.NewServer, below) and its upstream-facing leg
// (mcp.NewClient, broker.go's connectUpstream) advertise. Version is resolved
// fresh from internal/version.Version on every call rather than copied once
// into a package-level constant -- this constant used to be a hardcoded
// "0.3.0" that silently drifted while the product shipped v1.25.0
// (fable-audit §7 #15); resolving it live from the single shared source
// makes that class of drift structurally impossible, and is what
// version_test.go's TestGatewayImplementationTracksVersion asserts against.
func gatewayImplementation() *mcp.Implementation {
	return &mcp.Implementation{Name: "orbeat-gateway", Version: version.Version}
}

// Server is the gateway's dependencies and HTTP surface.
type Server struct {
	store      *store.Store
	resolver   *authz.Resolver
	verifier   mcpauth.TokenVerifier
	secrets    *secrets.Resolver
	resource   string
	authServer string
	sessions   *sessionCache
	keepAlive  time.Duration
	// sessionTransportTimeout configures the SDK's own transport-session idle
	// reclamation (Handler()'s mcp.StreamableHTTPOptions.SessionTimeout). New
	// sets this to sessionMaxAge, unchanged, matching the max-age this
	// server's own session cache already rebuilds at. It is a field rather
	// than a bare reference to the sessionMaxAge constant inside Handler() so
	// server_test.go can construct a Server directly with a short test-only
	// value and observe the SDK's real reclamation behavior end to end,
	// instead of waiting out the production duration.
	sessionTransportTimeout time.Duration
	logger                  *slog.Logger
	metrics                 *telemetry.Metrics
	// tracer emits the span registerProxies wraps around each proxied upstream
	// tools/call (fable-audit §7 #14) -- the uninstrumented latency between the
	// gateway's own http.server span (otelhttp, Handler()) and pgx's db.query
	// spans. Built once from the global provider in New() (a struct field, not
	// a bare otel.Tracer(...) call inside the hot path) so tests can inject a
	// real, recording tracer directly -- mirrors telemetry.queryTracer's tr
	// field, the same DI shape this codebase already uses for span testing.
	tracer trace.Tracer

	// limiter bounds the per-principal rate of tools/call (spec §4.2); nil
	// (New's default) means limiting is disabled -- every existing test that
	// constructs a Server directly, or via New without configuring one,
	// behaves exactly as before this slice. cmd/gateway wires this from
	// ORBEAT_RATELIMIT_RPS/ORBEAT_RATELIMIT_BURST via SetLimiter.
	limiter *ratelimit.Limiter
	// initLimiter bounds the per-principal RATE of session-creating
	// "initialize" calls, on its own (typically lower) budget, separate from
	// limiter's tools/call budget (spec §4.3). Also nil by default.
	// cmd/gateway wires this from ORBEAT_RATELIMIT_INIT_RPS via
	// SetInitLimiter.
	initLimiter *ratelimit.Limiter
	// inflight caps how many tools/call a single principal may have IN FLIGHT
	// at once -- a different axis from limiter, which bounds calls per second.
	// A caller inside its rate budget can still hold many simultaneous long
	// calls, each pinning a goroutine and a live upstream request. nil means
	// unlimited, matching limiter's zero value.
	inflight *ratelimit.ConcurrencyLimiter
	// revocations is the per-call virtual-key revocation cache (rbac_middleware.go,
	// docs/specs/2026-08-25-orbeat-virtual-keys-design.md §8). It is a plain
	// map/mutex type with no Enterprise dependency (never nil, unlike
	// limiter/initLimiter/inflight above -- there is no "disabled" state for a
	// revocation check), so Server.keyRevoked (virtualkey.ee.go, stubbed in
	// virtualkey.community.go) is the only Enterprise-gated part of this
	// mechanism.
	revocations *revocationCache
	// interceptor is the runtime call-interception scanner (docs/specs/2026-08-25-
	// orbeat-runtime-interception-design.md), applied on BOTH directions of a
	// tools/call: arguments, by rbacMiddleware (interceptArguments,
	// intercept.go) after the RBAC decision and before the call reaches the
	// upstream; and results, by registerProxies's proxy closure
	// (interceptResult, intercept.go, called from broker.go) after the
	// upstream has already returned -- the two directions are NOT symmetric
	// in what blocking them buys (see interceptResult's doc comment). nil
	// (New's default, and every existing test that builds a Server without
	// configuring one) means neither hook runs at all -- no scan, no latency,
	// no behaviour change, mirroring limiter/inflight's nil-safe pattern
	// above. cmd/gateway wires this from ORBEAT_INTERCEPT via SetInterceptor
	// (Task 4). It is deliberately the SAME govern.Scanner type artifact
	// submission uses (internal/api's s.scanner), so plugging in the same
	// CompositeScanner -- rule scanner plus the optional advisory LLM layer --
	// reuses that layer's already-established block/warn clamping without
	// this package knowing anything about LLM scanning.
	interceptor govern.Scanner

	// usage counts ALLOWED, FORWARDED tool calls per (subject, server, tool,
	// role) in memory, flushed to Postgres off the hot path (docs/specs/
	// 2026-08-25-orbeat-usage-metering-design.md section 3). nil (New's
	// default) means metering is disabled -- every existing test that
	// constructs a Server without configuring one behaves exactly as before
	// this slice, mirroring interceptor/limiter's nil-safe pattern above.
	// cmd/gateway wires this via SetUsageCounter (a later task's ticker
	// wiring, not built here).
	usage *UsageCounter
	// quota enforces per-role monthly call caps against usage's cached
	// totals (docs/specs/2026-08-25-orbeat-usage-metering-design.md section
	// 2). nil (New's default) means quota enforcement is disabled --
	// mirrors usage's nil-safe "absent = off" contract. cmd/gateway wires
	// this via SetQuotaEnforcer (a later task's ticker wiring, not built
	// here).
	quota *QuotaEnforcer
}

// New builds a gateway Server. verifier validates downstream bearer tokens;
// resource is this gateway's public URL; authServer is the Keycloak issuer
// advertised in protected-resource metadata.
func New(s *store.Store, r *authz.Resolver, verifier mcpauth.TokenVerifier, sec *secrets.Resolver, resource, authServer string) *Server {
	meter := otel.Meter("orbeat-gateway")
	metrics := telemetry.NewMetrics(meter)
	sessions := newSessionCache(sessionTTL, sessionMaxAge, metrics)
	if err := telemetry.RegisterSessionGauge(meter, func() int64 { return int64(sessions.size()) }); err != nil {
		slog.Warn("otel session gauge", "err", err) // non-fatal, mirrors cmd/*'s RegisterPoolGauges callers
	}
	return &Server{
		store: s, resolver: r, verifier: verifier, secrets: sec,
		resource: resource, authServer: authServer,
		sessions:                sessions,
		keepAlive:               defaultUpstreamKeepAlive,
		sessionTransportTimeout: sessionMaxAge,
		logger:                  slog.Default(),
		metrics:                 metrics,
		tracer:                  otel.Tracer("orbeat-gateway"),
		revocations:             newRevocationCache(),
	}
}

// SetLimiter overrides the per-principal tools/call rate limiter (spec §4.2).
// Default nil (limiting disabled) -- mirrors the API adapter's nil-safe
// Limiter (rate-limiting plan Task 4), so no existing test constructing a
// Server directly needs to change. cmd/gateway wires this from
// ORBEAT_RATELIMIT_RPS/ORBEAT_RATELIMIT_BURST.
func (s *Server) SetLimiter(l *ratelimit.Limiter) { s.limiter = l }

// SetInitLimiter overrides the separate, typically-lower-budget limiter
// bounding the RATE of session-creating "initialize" calls (spec §4.3) --
// distinct from SetLimiter's tools/call budget. Default nil (unlimited).
// cmd/gateway wires this from ORBEAT_RATELIMIT_INIT_RPS.
func (s *Server) SetInitLimiter(l *ratelimit.Limiter) { s.initLimiter = l }

// SetInflightCap overrides the per-principal concurrency cap on tools/call
// (fable-audit §7 #14). Default nil (unlimited).
func (s *Server) SetInflightCap(c *ratelimit.ConcurrencyLimiter) { s.inflight = c }

// SetInterceptor installs the runtime call-interception scanner (see the
// interceptor field's doc comment). Default nil (the hook never runs).
// cmd/gateway calls this only when ORBEAT_INTERCEPT is set.
func (s *Server) SetInterceptor(sc govern.Scanner) { s.interceptor = sc }

// SetUsageCounter installs the in-process usage-metering counter (see the
// usage field's doc comment). Default nil (the hook never runs, and no
// usage_daily row is ever written).
func (s *Server) SetUsageCounter(uc *UsageCounter) { s.usage = uc }

// SetQuotaEnforcer installs the per-role monthly quota enforcer (see the
// quota field's doc comment). Default nil (the check never runs, and no
// role is ever denied for being over quota).
func (s *Server) SetQuotaEnforcer(qe *QuotaEnforcer) { s.quota = qe }

// Close drains the session cache, closing every cached subject's upstream MCP
// connections, and stops the rate limiters' background sweepers (if
// configured). Call it on gateway shutdown (or in tests) so nothing long-lived
// is leaked. It is safe to call more than once.
func (s *Server) Close() {
	s.sessions.closeAll()
	if s.limiter != nil {
		_ = s.limiter.Close()
	}
	if s.initLimiter != nil {
		_ = s.initLimiter.Close()
	}
	if s.inflight != nil {
		_ = s.inflight.Close()
	}
}

// Handler returns the gateway's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpx.HealthHandler())
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)

	mcpHandler := mcp.NewStreamableHTTPHandler(s.getServer, &mcp.StreamableHTTPOptions{
		// s.sessionTransportTimeout is sessionMaxAge in production (set once,
		// in New) -- reused, not a fresh literal. Without it SessionTimeout
		// defaulted to its zero value and idle SDK transport sessions were
		// NEVER reclaimed: resident until process restart (go-sdk@v1.7.0
		// mcp/streamable.go:166-172, 721-725; gateway session lifecycle design,
		// 2026-08-16 §2-3). It proves we asked the SDK to reclaim idle
		// transport sessions, not that the SDK's process memory shrinks (§6) --
		// that map lives in a dependency this package cannot read.
		//
		// THIS DOES NOT KEEP THE TWO SESSION LIFETIMES IN STEP, and the comment
		// here used to claim it did. sessionMaxAge is ABSOLUTE from builtAt
		// while SessionTimeout is IDLE, so an ACTIVE client's gateway session
		// is rebuilt while its transport session is still nowhere near expiry
		// -- exactly the divergence A1 rides on. withSession's binding check is
		// what closes it; this value is a reclamation bound for transport
		// sessions whose client walked away, and nothing more.
		//
		// The equality with sessionMaxAge is still worth pinning, but for a
		// DIAGNOSTIC reason rather than the safety one this comment used to
		// give. sweepTransportsLocked forgets a tombstoned binding one
		// tombstoneHorizon (2*maxAge) after it was created; the value below is
		// what bounds how long the SDK can still be holding the transport
		// session that tombstone describes, and therefore how long a 404 for
		// it should still be able to say WHY. It is no longer what stands
		// between a replayed id and the frozen *mcp.Server -- withSession
		// refuses an id it holds no binding for, so a swept tombstone and a
		// live one both end in 404. TestServerSessionTransportTimeoutFieldMat
		// chesSessionMaxAge pins BOTH halves of what the horizon reads -- this
		// field AND the cache's own ttl/maxAge -- because the horizon is
		// computed from the cache, not from here. Until 2026-08-29 only this
		// field was pinned, and a cache built with sessionMaxAge/4 left the
		// whole package green. See tombstoneHorizon for the full argument.
		SessionTimeout: s.sessionTransportTimeout,
	})
	opts := &mcpauth.RequireBearerTokenOptions{ResourceMetadataURL: s.resource + "/.well-known/oauth-protected-resource"}
	protected := mcpauth.RequireBearerToken(s.verifier, opts)(s.withSession(mcpHandler))
	mux.Handle("/mcp", protected)
	// withIdentityCarrier must sit OUTSIDE logging.Requests (fable-audit §7
	// #14): it installs the mutable *requestIdentity that withSession fills in
	// as it learns each piece of the caller's identity, and gatewayIdentity
	// reads back -- see identity.go's doc comment for why a plain non-nil
	// identity func (the internal/api pattern) can't work here: withSession's
	// context.WithValue additions run strictly downstream of the point
	// logging.Requests snapshots its own ctx, so they're invisible to it
	// without a shared mutable pointee installed beforehand.
	return otelhttp.NewHandler(withIdentityCarrier(logging.Requests(s.logger, gatewayIdentity)(mux)), "http.server")
}

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	authServers := []string{}
	if s.authServer != "" {
		authServers = []string{s.authServer}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resource":              s.resource,
		"authorization_servers": authServers,
	})
}

// sessionCtxKey carries the prebuilt session from withSession to getServer.
type sessionCtxKey struct{}

const (
	// mcpSessionIDHeader is the Streamable-HTTP session header (MCP spec
	// §2.5). The SDK's own constant (go-sdk@v1.7.0
	// mcp/streamable_headers.go:25) is unexported, so it is restated here --
	// the string is fixed by the spec, not by the SDK.
	mcpSessionIDHeader = "Mcp-Session-Id"
	// sessionRebuiltHeader marks a 404 written by THIS gateway (a transport
	// session whose gateway session was reclaimed, or an id it holds no
	// binding for) rather than by the SDK's lookupSession. Both reach the
	// client as an error wrapping mcp.ErrSessionMissing and are therefore
	// indistinguishable from the client side -- so without this marker
	// TestHandlerActuallyReclaimsIdleTransportSessions would pass whether or
	// not the SDK ever reclaimed anything.
	//
	// Its VALUE is a small closed set, and the enumeration is exhaustive: the
	// five reclaim causes session.go defines (max_age, idle_timeout, dirty,
	// entitlement_change, explicit_close), plus reclaimUnknown's "unknown"
	// wherever withSession refuses a binding that no eviction path left a cause
	// on, plus sessionUnbound below. It used to list only the first five while
	// withSession could already write the sixth (found in review, 2026-08-30).
	//
	// sessionCrossSubject below is deliberately NOT in that set: it is a log
	// cause and never a header value, for the reason its own comment gives.
	sessionRebuiltHeader = "X-Orbeat-Session-Rebuilt"
	// sessionUnbound is the marker value for an inbound Mcp-Session-Id this
	// process holds NO binding for. It is deliberately not one of session.go's
	// reclaim causes: nothing was reclaimed, the gateway simply has no record,
	// and an operator reading "unbound" is being told something different from
	// "max_age" -- a previous process minted it, a tombstone was swept, or it
	// was never real.
	sessionUnbound = "unbound"
	// sessionCrossSubject is a LOG-ONLY cause: one authenticated principal
	// replaying an Mcp-Session-Id minted for a DIFFERENT principal, whose
	// gateway session is alive and simply is not this caller's. Every other
	// value in this file and in session.go's reclaim set describes something
	// the GATEWAY did to a session; this one describes something a CLIENT did.
	// Folding it into reclaimUnknown made a replay attempt read in the log
	// exactly like the internal bind-after-eviction race that constant names,
	// which is how it was reported before this cause existed (review,
	// 2026-08-30).
	//
	// It never reaches the wire. The header goes on saying "unknown" for this
	// case, byte-identical to what it said before the distinction existed,
	// because a caller who could tell "this id names a live session that is not
	// yours" from "this id was never minted here" would hold a liveness oracle
	// over other people's session ids. The operator gets the finer answer; the
	// replayer gets the same 404 as everyone else.
	sessionCrossSubject = "cross_subject"
	// sessionRebuiltBody is deliberately not the SDK's "session not found",
	// for the same reason the header exists.
	sessionRebuiltBody = "gateway session rebuilt; start a new MCP session"
	// requestIDHeader is internal/logging's own request-id header name,
	// restated here for the same reason mcpSessionIDHeader is: no exported
	// constant to import (logging/middleware.go hardcodes the literal too).
	// withSession stamps it onto the INBOUND *http.Request before handing
	// control to the SDK, so request_id.go's perCallRequestID can recover it
	// per call from RequestExtra.Header -- see that file's doc comment for
	// why (fable-audit B13).
	requestIDHeader = "X-Request-Id"
)

// withSession resolves the caller's gateway session BEFORE the SDK handler
// runs (audit G11). The SDK maps a nil *mcp.Server from getServer to
// 400 Bad Request -- wrong for a server-side build failure (store/resolver
// outage): clients like Claude Code treat 400 as permanent and won't retry.
// Building here lets a failed build surface as 503 + Retry-After (a retryable
// server fault) while getServer just reads the prebuilt session from context.
// Requests without a principal pass through session-less; getServer then
// returns nil → the SDK's 400 (the bearer middleware wrapping this one
// normally makes that case unreachable).
//
// It is also where the two session lifetimes are ENFORCED against each other
// (A1; the binding itself is taken at mint time by buildSession's
// GetSessionID closure). The SDK consults getServer exactly once per transport
// session -- on the initialize, the one request that carries no
// Mcp-Session-Id -- and every later request goes straight to the stored
// transport, so once our gateway session is evicted and rebuilt the client
// keeps talking to a frozen *mcp.Server holding revoked entitlements and
// closed upstreams. The SDK exposes no server-side way to terminate that
// transport session (StreamableHTTPHandler.closeAll is unexported), so the 404
// the MCP spec prescribes for a terminated session (§2.5.3; the SDK client
// maps it to mcp.ErrSessionMissing, streamable.go:2533-2539) has to be written
// HERE, before the request reaches mcpHandler.
//
// AN ID THIS PROCESS HOLDS NO BINDING FOR IS REFUSED, NOT ADOPTED. The branch
// that used to bind an unknown id to the current session and let it through
// justified itself with "double-404ing would break every legitimate client
// after a gateway restart". That was false, and sessionRebuiltHeader's own
// comment is what disproves it: the gateway's 404 and the SDK's 404 both reach
// the client as an error wrapping mcp.ErrSessionMissing and are
// indistinguishable client-side, so both make it re-initialize. After a
// restart the SDK's session map is empty too, so the outcome is identical
// either way -- the branch bought nothing and cost a governance hole.
//
// What it cost: a client could hold a POST open indefinitely (the SDK's
// subscriptions/listen blocks on <-ctx.Done() whenever a subscription is
// agreed, which it is for any server with tools, and rbacMiddleware gates only
// tools/call), wait for its gateway session to be reclaimed AND for the
// tombstone to be swept, then replay the id on an ordinary POST. The unknown
// branch would rebind it and hand the request to the frozen *mcp.Server:
// tools/list advertising revoked tools, rbacMiddleware writing an "allow" from
// the pre-revocation snapshot. With mint-time binding, an unknown id can only
// come from a previous process, a swept tombstone, or a forgery, and 404 is
// the right answer to all three.
//
// WHAT THIS SLICE DID NOT CLOSE, AND WHAT SINCE CLOSED IT. The paragraph above
// is about a LATER request replaying the id; a subscriptions/listen POST
// admitted BEFORE the eviction was a different thing and survived this slice
// untouched. It went on blocking on <-ctx.Done(), keeping the frozen
// *mcp.Server, its rbacMiddleware closure over the revoked snapshot and its
// []*upstreamConn reachable from that one goroutine, with nothing bounding how
// long: cmd/gateway/main.go sets ReadHeaderTimeout alone, and the SDK's
// startPOST stops the idle timer for the POST's whole duration, resetting it
// only in endPOST, deferred until the response completes.
//
// Closed 2026-09-01 by boundListen (listen_bound.go), a per-method cap at
// sessionMaxAge added to this session's own middleware chain. Per-method and
// not a WriteTimeout on the http.Server, because that would bound every
// legitimate long-running tools/call, which is the defect v1.17.0 removed
// ResponseHeaderTimeout to fix.
//
// It is resource RETENTION and not a governance bypass, and the difference is
// that the held POST forwards no call and reaches no upstream:
// subscriptions/listen blocks rather than dispatching, this gateway registers
// no resources and no SubscribeHandler, and a client cannot inject a new
// request into a POST body it has already sent. Anything it sends afterwards
// is a new request carrying the Mcp-Session-Id, and the switch below refuses
// it. That distinction is why the cap could be a bounded hold rather than a
// refusal: nothing had to be taken away from a caller to close it.
//
// DELETE IS THE ONE EXEMPTION. serveStatefulDELETE is the only mechanism that
// promptly reclaims an SDK transport session, and refusing a stale-id DELETE
// left the evicted session's frozen *mcp.Server, its rbacMiddleware closure
// over the revoked snapshot and its []*upstreamConn resident for the full
// SessionTimeout even when the client politely said goodbye. Letting it
// through closes the transport, which is exactly what the tombstone wants, and
// the tombstone is NOT consumed by it, so a later POST replay is still
// refused. It is not a hijacking surface either: the SDK's own lookupSession
// answers 403 when the bearer token's UserID does not match the one that
// created the session (streamable.go), and verifier.go sets that UserID to the
// principal's subject.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromTokenInfo(mcpauth.TokenInfoFromContext(r.Context()))
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// Recorded as soon as the bearer token's Principal is recovered -- true
		// even if session build below subsequently fails -- so a 503 still logs
		// WHO the failing caller was (fable-audit §7 #14). No-op if ctx carries
		// no identity carrier (a caller of withSession that bypassed Handler()'s
		// withIdentityCarrier, e.g. some unit tests).
		recordSubject(r.Context(), p.Subject)
		// Restamp the resolved-by-logging.Requests request id onto r.Header
		// itself (fable-audit B13, see request_id.go's package doc comment):
		// the SDK repopulates RequestExtra.Header fresh from req.Header on
		// EVERY POST, including every later tools/call sharing this transport
		// session, so this is what lets rbacMiddleware recover the CURRENT
		// call's own id instead of the one frozen on the jsonrpc2 connection's
		// ctx. r.Header is the same map every *http.Request derived from r via
		// WithContext shares by reference, so this mutation is visible all the
		// way down to the SDK's own servePOST without needing ctx at all.
		r.Header.Set(requestIDHeader, logging.RequestID(r.Context()))
		sess, err := s.sessions.getOrBuild(ratelimit.KeyFor(p), time.Now(), func() (*session, error) {
			// WithoutCancel: the session (and its upstream SSE streams) must outlive
			// the HTTP request that happened to trigger the build -- and a canceled
			// request must not poison the single-flight result shared with other
			// callers. Trace/log values are preserved.
			return s.buildSession(context.WithoutCancel(r.Context()), p)
		})
		if err != nil {
			s.logger.Warn("build session", "subject", p.Subject, "err", err.Error())
			w.Header().Set("Retry-After", "5")
			http.Error(w, "gateway session unavailable", http.StatusServiceUnavailable)
			return
		}
		// Tenant is only knowable once a session (cached or freshly built)
		// exists -- recorded BEFORE the 404 branch below as well as on the
		// success path, so a rebuilt-session rejection still logs a subject
		// WITH its tenant (the shape identity_test.go's
		// TestRequestLogCarriesIdentity asserts).
		recordTenant(r.Context(), sess.tenantID)

		ctx := context.WithValue(r.Context(), sessionCtxKey{}, sess)
		inbound := r.Header.Get(mcpSessionIDHeader)
		b := s.sessions.lookupTransport(inbound)
		// Every case is named, including the two that do nothing. The chain
		// this replaced put an `else if` after a `return` and left the
		// already-bound case implicit, which read as an oversight rather than
		// as a decision.
		switch {
		case inbound == "":
			// The initialize (or a pre-session ping). The SDK mints the id
			// inside getServer's very next statement and buildSession's
			// GetSessionID closure binds it there; nothing to check yet.
		case r.Method == http.MethodDelete:
			// Session termination. Let it reach serveStatefulDELETE whatever
			// the binding says -- see the DELETE exemption above.
		case b.known && b.sess == sess:
			// Bound to the caller's CURRENT session: the ordinary hot path.
		case b.known:
			// Bound, but not to the session serving this request. Three shapes
			// reach here and the CLIENT gets the same 404 for all three; only
			// the log tells them apart.
			//
			//   - b.sess == nil: a tombstone, so some eviction path already
			//     recorded why and b.cause names it.
			//   - b.sess is this subject's own but no longer current: the
			//     reclaimUnknown race, bindTransport landing just after an
			//     eviction. Nothing recorded a cause, which is what the
			//     fallback below is for.
			//   - b.sess belongs to ANOTHER subject: a replay of somebody
			//     else's id. Nothing was rebuilt and that session is alive.
			rej := markedRejection(b.cause, "gateway session rebuilt")
			if rej.header == "" {
				// Without this the 404 ships an empty X-Orbeat-Session-Rebuilt
				// and the log line says cause="", which is exactly the
				// operator dead end tombstone retention exists to prevent.
				rej = markedRejection(reclaimUnknown, rej.why)
			}
			if b.sess != nil && b.sess.subject != p.Subject {
				// Compared on SUBJECT, not on session identity: b.sess != sess
				// is also true for the same subject's superseded session, and
				// calling that a replay would be a false accusation in the one
				// place an operator goes to find a real one.
				rej.cause, rej.why = sessionCrossSubject, "transport session replayed by another subject"
			}
			s.reject(w, p.Subject, inbound, rej)
			return
		default:
			// No binding at all. A previous process, a swept tombstone, or a
			// forgery -- 404 for all three.
			s.reject(w, p.Subject, inbound, markedRejection(sessionUnbound, "no binding for this transport session"))
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// rejection is the answer withSession gives a transport session it will not
// serve. header and cause are separate fields on purpose: header is what the
// CLIENT reads and its value set must stay exactly what it was before the
// cross-subject replay was told apart, while cause is what the OPERATOR reads
// in the log, where that distinction is the entire point. markedRejection
// builds the ordinary case where the two are the same string.
type rejection struct {
	header string // X-Orbeat-Session-Rebuilt value; a small closed set.
	cause  string // cause= in the log line; may be finer-grained than header.
	why    string // the log message, appended to a fixed prefix by reject.
}

func markedRejection(cause, why string) rejection {
	return rejection{header: cause, cause: cause, why: why}
}

// reject writes the gateway's own 404 for a transport session it will not
// serve, carrying rej.header in sessionRebuiltHeader so an operator holding
// nothing but a curl can tell it from the SDK's unmarked "session not found".
func (s *Server) reject(w http.ResponseWriter, subject, inbound string, rej rejection) {
	s.logger.Info("mcp transport session rejected: "+rej.why,
		"subject", subject, "mcp_session_id", inbound, "cause", rej.cause)
	w.Header().Set(sessionRebuiltHeader, rej.header)
	http.Error(w, sessionRebuiltBody, http.StatusNotFound)
}

// getServer is invoked by the SDK ONLY for a request carrying no
// Mcp-Session-Id -- in practice the initialize, once per transport session
// (go-sdk@v1.7.0 mcp/streamable.go:633-653: serveStatefulPOST calls
// h.getServer(req) only on that branch, and every later POST/GET/DELETE goes
// through lookupSession straight to the stored transport). The *mcp.Server it
// returns is therefore FROZEN for that transport session's life, together with
// the rbacMiddleware closed over the session snapshot and the proxies
// registered against that snapshot's upstream connections.
//
// What keeps it fresh is not this function: it is withSession's binding check,
// which 404s any request whose Mcp-Session-Id was built from a gateway session
// that has since been reclaimed, so the client re-initializes and lands back
// here against the current one.
//
// withSession has already built (or fetched) the caller's session. Returning
// nil → the SDK's 400, kept only for the no-principal case.
func (s *Server) getServer(r *http.Request) *mcp.Server {
	sess, ok := r.Context().Value(sessionCtxKey{}).(*session)
	if !ok {
		return nil
	}
	return sess.mcpServer
}

// buildSession resolves entitlements, connects entitled+reachable upstreams,
// registers namespaced passthrough proxies, and attaches the per-call RBAC
// middleware. Unreachable/unsupported upstreams are skipped + audited.
//
// Identity resolution forks first (docs/specs/2026-08-25-orbeat-virtual-keys-
// design.md §7): resolveVirtualKey (virtualkey.ee.go/virtualkey.community.go)
// is tried before s.resolver.Resolve because a virtual key must bypass
// everything Resolve does for a human -- checkSeatCap, UpsertUser, and
// reading roles from the token's p.Roles instead of the key's own row
// (internal/authz/resolver.go:82,85,89). ok is only true when p.ClientID
// matched a live virtual_key row for this tenant; on false (including every
// call in a Community build, where resolveVirtualKey's stub always returns
// ok=false) rc/keyID/keyNarrow stay zero and the existing human path below
// runs completely UNCHANGED, exactly as it did before this fork existed.
//
// The fork can also REFUSE, which it could not until 2026-08-30: a
// client_credentials token naming no virtual_key row comes back as
// errOrphanedServiceAccount and this function returns it, so the human path
// below never runs and neither does checkSeatCap or UpsertUser. Handled by
// the existing `kerr != nil` arm rather than by a new branch, because the
// desired behaviour, abort the build, is what that arm already did for a
// store failure. See resolveVirtualKey's own doc comment for why a robot
// falling through to the human path was the defect and why refusing it
// cannot catch a human.
func (s *Server) buildSession(ctx context.Context, p auth.Principal) (*session, error) {
	// Bound ONLY the DB-resolution phase. ctx itself must stay deadline-free:
	// each upstream's upCtx derives from it, and a deadline there would sever
	// live SSE streams when it fired.
	dbCtx, dbCancel := context.WithTimeout(ctx, sessionBuildDBTimeout)
	defer dbCancel()

	var rc authz.ResolvedContext
	var keyID string
	var keyNarrow []string
	if krc, kID, kNarrow, ok, kerr := s.resolveVirtualKey(dbCtx, p); kerr != nil {
		return nil, kerr
	} else if ok {
		rc, keyID, keyNarrow = krc, kID, kNarrow
	} else {
		var err error
		rc, err = s.resolver.Resolve(dbCtx, p)
		if err != nil {
			return nil, err
		}
	}

	ents, err := s.store.ListEntitlementsByRoles(dbCtx, rc.TenantID, rc.RoleIDs)
	if err != nil {
		return nil, err
	}
	visible := rbac.VisibleServerIDs(ents)
	servers, err := s.store.ListMCPServersByTenant(dbCtx, rc.TenantID)
	if err != nil {
		return nil, err
	}

	sess := &session{
		subject: p.Subject, tenantID: rc.TenantID, actor: p.Subject,
		entitlements: ents, slugToServer: map[string]string{},
		keyID: keyID, keyNarrow: keyNarrow,
	}
	// THE MINT-TIME BINDING (A1). Every gateway session owns its own
	// *mcp.Server, so this closure can name exactly one *session -- the one
	// whose frozen mcpServer will serve the transport session the SDK is about
	// to create. serveStatefulPOST calls getServer and then, on the very next
	// statement, server.opts.GetSessionID (go-sdk@v1.7.0 mcp/streamable.go, the
	// "No session ID: create a new session" branch), all before the transport
	// exists and long before the id is written into the response. So the
	// binding is in place BEFORE any client can learn the id, and there is no
	// window in which a minted id is unbound.
	//
	// It replaces a response-writer wrapper that read the id back out of the
	// response headers at flush time. That wrapper was removable with the whole
	// package still green -- the client's first post-initialize request found
	// the id unknown and bound it anyway -- which is precisely the branch the
	// gateway no longer has (withSession refuses an unknown id outright).
	//
	// Returning "" here would make the SDK create an EPHEMERAL, unaddressable
	// session, so this must always return an id. rand.Text is what the SDK's
	// own default (mcp.NewServer, ServerOptions.GetSessionID == nil) uses, and
	// picking the same generator keeps the id's shape and entropy unchanged.
	//
	// Deadlock note: bindTransport takes sessionCache.mu, and the SDK holds no
	// lock of its own across this call (the h.mu-guarded registration of the
	// session happens later, in serveStatefulPOST's connectOpts path).
	sess.mcpServer = mcp.NewServer(gatewayImplementation(), &mcp.ServerOptions{
		GetSessionID: func() string {
			id := rand.Text()
			s.sessions.bindTransport(id, sess)
			return id
		},
	})
	if keyID != "" {
		// p.ClientID, not anything resolveVirtualKey returned: it already
		// proved a live virtual_key row exists for exactly this value (that
		// is how ok became true), and it is the identifier
		// store.GetVirtualKeyByClientID takes -- see keyClientID's doc comment
		// (session.go) for why keyID (the store row's uuid) cannot serve
		// this instead.
		sess.keyClientID = p.ClientID
	}

	// TWO PHASES, and the split is the whole design.
	//
	// Phase 1 dials every candidate CONCURRENTLY, because a session's build time
	// used to be the SUM of its upstreams: N servers meant up to N times
	// upstreamDialTimeout before the session was usable, and one slow upstream
	// delayed every other one behind it. Dialling is pure I/O (secret resolve,
	// DNS, TCP, TLS, MCP handshake) and touches nothing shared.
	//
	// Phase 2 then walks the results IN THE ORIGINAL SERVER ORDER and does every
	// ordering-sensitive thing sequentially: the slug-collision guard, tool
	// registration into the SDK server, and the appends to sess. That order is
	// not a preference. `servers` arrives sorted by name, and the collision
	// guard's "first one wins" is only a rule if "first" is deterministic; doing
	// it in completion order would hand the slug to whichever upstream happened
	// to answer fastest, which is a coin flip that decides WHOSE ENTITLEMENTS
	// authorize a tool (the v1.17.0 privilege-escalation fix). registerProxies
	// is sequential for a second reason: the SDK registry silently replaces
	// same-named tools, so concurrent registration would race over which
	// upstream owns a name.
	//
	// One behaviour genuinely changes, and it is an efficiency detail rather
	// than a safety one: a slug LOSER is now dialled before it is skipped,
	// where the sequential loop could skip it before opening anything. It has
	// to be, because "did the first collider connect" is not knowable until it
	// is dialled, and pre-computing winners from names alone would let a DEAD
	// first collider hold a slug hostage forever, which is worse. The loser's
	// connection is closed immediately and its tools are never registered, so
	// the property the guard exists for is untouched.
	var candidates []store.MCPServer
	for _, srv := range servers {
		if _, ok := visible[srv.ID]; !ok {
			continue
		}
		// Only "active" servers are live: skip non-active (draft/disabled/…) entries
		// even when entitled. This is an intentional skip, not a connect failure, so
		// it is NOT audited as an upstream-connect error -- silently continue, like the
		// not-visible case above.
		if srv.Status != "active" {
			continue
		}
		candidates = append(candidates, srv)
	}

	results := make([]dialOutcome, len(candidates))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelDials)
	for i, srv := range candidates {
		wg.Add(1)
		go func(i int, srv store.MCPServer) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.dialUpstream(ctx, srv)
		}(i, srv)
	}
	wg.Wait()

	for i := range results {
		r := results[i]
		srv := candidates[i]
		if r.reason != "" {
			// Audited here rather than inside the goroutine so the audit trail
			// keeps the deterministic order the sequential loop produced. An
			// audit is a DB write; ordering it by which upstream failed fastest
			// would make the same session build read differently every time.
			if r.watchdog != nil {
				s.skipUpstream(ctx, sess, srv.ID, r.watchdog, r.cancel, r.conn, r.reason, r.err)
				continue
			}
			s.logger.Warn("skip upstream", "server_id", srv.ID, "reason", r.reason)
			s.audit(ctx, sess, "gateway.upstream.connect", srv.ID, "error")
			continue
		}
		// Slug-collision guard (audit G3): naming.Slugify is lossy, so distinct
		// server names can share a slug. Without this check the last writer would
		// silently overwrite slugToServer while the earlier server's tools stayed
		// registered -- per-call RBAC would then authorize those tools against the
		// LATER server's entitlements. The first server to connect successfully
		// wins, and this loop's name order is what makes "first" deterministic; a
		// dead first collider correctly cedes the slug, because a failed dial
		// never reaches this line. The loser's tools can never clobber the
		// winner's in the SDK registry (which silently replaces same-named
		// tools), because registerProxies below runs only for the winner.
		// The api rejects colliding names at create/update, but that check is
		// advisory-pre-tx -- this guard is the backstop.
		if winnerID, taken := sess.slugToServer[naming.Slugify(srv.Name)]; taken {
			s.logger.Error("slug collision",
				"server_id", srv.ID, "name", srv.Name,
				"slug", naming.Slugify(srv.Name), "winner_server_id", winnerID)
			s.skipUpstream(ctx, sess, srv.ID, r.watchdog, r.cancel, r.conn, "slug collision", nil)
			continue
		}
		if _, err := registerProxies(r.upCtx, sess.mcpServer, r.conn, sess.markDirty, s.tracer, s, sess); err != nil {
			s.skipUpstream(ctx, sess, srv.ID, r.watchdog, r.cancel, r.conn, "register tools", err)
			continue
		}
		if !r.watchdog.Stop() {
			// Watchdog already fired: cancel() ran, so the upstream ctx (and the SSE
			// stream bound to it) is severed. Don't cache a dead conn -- treat as a
			// dial-timeout failure.
			s.skipUpstream(ctx, sess, srv.ID, r.watchdog, r.cancel, r.conn, "dial timeout (watchdog race)", nil)
			continue
		}
		r.conn.cancel = r.cancel
		sess.slugToServer[r.conn.slug] = srv.ID
		sess.upstreams = append(sess.upstreams, r.conn)
		s.audit(ctx, sess, "gateway.upstream.connect", srv.ID, "allow")
	}

	// ONE call, rate limiter(s) before rbacMiddleware in the argument list.
	// AddReceivingMiddleware(m1, m2, m3) composes m1(m2(m3(handler))) -- the
	// first argument runs outermost -- but a SEPARATE later call wraps
	// whatever handler is already installed, so last-registered would run
	// first. Splitting this into two calls reverses the effective order even
	// though each call individually reads "first-argument-outermost": a
	// throttled tools/call would then reach rbacMiddleware FIRST, which
	// writes an "allow" audit row (spec §4.2) for a call that never actually
	// executes, on the governance surface this repo audits deny/allow
	// decisions on. Pinned by internal/gateway/ratelimit_test.go, not left to
	// this comment.
	// fable-audit B38(b), ANALYSIS ONLY -- NOT fixed here. s.initLimiter is
	// registered on sess.mcpServer, and sess.mcpServer does not exist until
	// buildSession has already run to completion: everything above this
	// line -- the DB-resolution phase, entitlement/server listing, and up to
	// maxParallelDials=8 concurrent upstream dials -- happens BEFORE the
	// init limiter ever gets a chance to reject anything. A caller hammering
	// "initialize" pays the full session-build cost on every attempt that
	// misses the cache, and only the (already-built) session's OWN limiter
	// throttles it, one build too late.
	//
	// Moving the check earlier is a real fix, not a one-line move, and is
	// left undone rather than half-done:
	//   - withSession (this file, above) is the only place that runs BEFORE
	//     getOrBuild/buildSession, but it operates on the raw *http.Request
	//     and never parses the JSON-RPC body, so it cannot know the inbound
	//     method is "initialize" -- only that inbound == "" (no
	//     Mcp-Session-Id yet), which also covers a GET/ping and, per the
	//     switch above, a handful of other pre-session cases.
	//   - The correct rate-limit key at that point has to come from p
	//     (auth.Principal) directly -- there is no req/extra yet to read
	//     sessionKeyFn's per-call TokenInfo from, so this would need its own
	//     key derivation, not a reuse of sessionKeyFn.
	//   - A rejection at that layer is a raw HTTP response, not a JSON-RPC
	//     error -- a different reject vocabulary from ratelimit.MCP's
	//     (mirroring how the API adapter's own 429 differs from the
	//     gateway's JSON-RPC-error shape), and it has to compose correctly
	//     with the existing 503/404 branches below and in withSession.
	//   - getOrBuild's singleflight already dedupes CONCURRENT duplicate
	//     builds for one key to a single buildSession call, so the risk this
	//     limiter is actually defending against is a caller (or many
	//     distinct callers) issuing many SEQUENTIAL session-creating
	//     requests over time, not a concurrent burst -- worth stating
	//     because it changes what "fixed" would even need to demonstrate.
	// Given all four, this is a structural relocation (raw-HTTP-layer
	// rate limiting ahead of session build, not method-handler-layer),
	// not a reordering of two lines, and is reported rather than
	// partially applied.
	rlObs := ratelimit.Observability{Metrics: s.metrics, Logger: s.logger}
	sess.mcpServer.AddReceivingMiddleware(
		ratelimit.MCP(s.limiter, "tools/call", sessionKeyFn, rlObs),
		ratelimit.MCP(s.initLimiter, "initialize", sessionKeyFn, rlObs),
		ratelimit.MCPConcurrency(s.inflight, "tools/call", sessionKeyFn, rlObs),
		// Bounds the one method that blocks instead of dispatching. See
		// boundListen's doc comment (listen_bound.go) for why this is a
		// per-method cap and not a WriteTimeout on the http.Server.
		boundListen(listenMethod, listenMaxHold),
		s.rbacMiddleware(sess),
	)

	return sess, nil
}

// sessionKeyFn derives a ratelimit key from the PER-CALL principal carried in
// req.GetExtra().TokenInfo, never from ctx -- see ratelimit.KeyFunc's doc.
// ctx is accepted (ratelimit.KeyFunc's shape) but deliberately unused:
// reading mcpauth.TokenInfoFromContext(ctx) here instead of extra.TokenInfo
// is exactly the mistake this function exists to not make.
//
// WHY, given fable-audit B14 (session.go, sessionCache now keyed on
// ratelimit.KeyFor(p) rather than bare subject): with that fix in place,
// withSession's mint-time binding (A1, this file) refuses any later request
// whose principal resolves to a DIFFERENT session than the one bound to its
// Mcp-Session-Id -- so on the ordinary path a transport session's every call
// already carries the SAME (subject, azp) the session was built from, and
// ctx (frozen at connection-creation) and extra.TokenInfo (read fresh per
// call) report the identical ratelimit.KeyFor value. Reading ctx would not
// misattribute a REACHABLE call today.
//
// This function still reads extra.TokenInfo anyway, and that is deliberate,
// not leftover caution: ctx is frozen at CONNECTION creation, before
// anything about THIS call has been resolved, so a ctx-based key depends
// entirely on withSession's binding check continuing to hold that invariant
// -- a defense living one layer away, in a different file, for a reason
// (transport-session integrity) that has nothing to do with rate limiting.
// Reading the value this call's own dispatch actually carries keeps
// sessionKeyFn correct on its own terms, not only as a consequence of A1
// remaining intact.
func sessionKeyFn(ctx context.Context, req mcp.Request) (string, bool) {
	_ = ctx
	extra := req.GetExtra()
	if extra == nil {
		return "", false
	}
	p, ok := principalFromTokenInfo(extra.TokenInfo)
	if !ok {
		return "", false
	}
	return ratelimit.KeyFor(p), true
}

// skipUpstream tears down a failed upstream dial attempt and audits the skip:
// disarm the watchdog, close the half-built conn (nil when connect itself
// failed), release the per-upstream ctx, warn, and write the error audit. One
// owner for the exit path keeps the teardown ordering from desyncing across
// the three failure branches.
// dialOutcome is one upstream's phase-1 result: either a live connection with
// the context and watchdog the caller must take ownership of, or a reason to
// skip it. It carries no decision about the OTHER upstreams, which is exactly
// what makes phase 1 safe to run concurrently.
type dialOutcome struct {
	conn     *upstreamConn
	upCtx    context.Context
	cancel   context.CancelFunc
	watchdog *time.Timer
	reason   string // non-empty => skip this upstream, with this reason
	err      error
}

// maxParallelDials bounds phase 1. Unbounded, a principal entitled to a large
// catalog would open every upstream connection at once on every session build,
// which is a burst this gateway inflicts on OTHER people's servers. Eight is
// enough that the common case (a handful of upstreams) is fully parallel while
// a large one degrades to batches rather than to a thundering herd.
const maxParallelDials = 8

// dialUpstream does the I/O half of bringing up one upstream: resolve its
// secrets and connect. It touches nothing shared, which is what makes it safe
// to run concurrently; every decision that depends on the other upstreams
// (slug ownership, tool registration) stays in the caller's sequential pass.
//
// It never audits and never logs a skip: the caller does both, in order. A
// goroutine that audited its own failure would write the trail in whatever
// order the network answered.
func (s *Server) dialUpstream(ctx context.Context, srv store.MCPServer) dialOutcome {
	if srv.Transport != "http" && srv.Transport != "sse" {
		return dialOutcome{reason: "unsupported transport"}
	}
	secret, err := s.secrets.Resolve(ctx, srv.SecretRef)
	if err != nil {
		// err is a ref/scheme/"is not set" error, never the secret value.
		return dialOutcome{reason: "secret resolve", err: err}
	}
	var caPEM string
	if srv.TLSCARef != "" {
		caPEM, err = s.secrets.Resolve(ctx, srv.TLSCARef)
		if err != nil {
			// err is a ref/scheme error, never the resolved bytes.
			return dialOutcome{reason: "tls ca resolve", err: err}
		}
	}
	// Per-upstream cancellable ctx with a cancel-on-failure watchdog: the ctx
	// must outlive the dial (the SSE hanging GET is bound to it), so a plain
	// WithTimeout would sever the live stream when its timer fired. The
	// watchdog cancels only if connect+register overruns upstreamDialTimeout;
	// on success it is disarmed by the caller and cancel is handed to the session.
	upCtx, cancel := context.WithCancel(ctx)
	watchdog := time.AfterFunc(upstreamDialTimeout, cancel)
	connectStart := time.Now()
	conn, err := connectUpstream(upCtx, srv, secret, caPEM, s.keepAlive)
	// Measured here, not inside connectUpstream (broker.go), which is a pure
	// dial function with no metrics handle: this is DNS+TCP+TLS+handshake as
	// one number, what an operator actually pays for this upstream. Recorded
	// on BOTH outcomes -- a histogram that only saw successes would hide the
	// upstream that is slow *and then* fails, which is exactly the incident
	// this metric exists to help explain (design
	// 2026-08-18-orbeat-gateway-connect-metrics-design.md §4).
	connectOutcome := "ok"
	if err != nil {
		connectOutcome = "error"
	}
	s.metrics.UpstreamConnect.Record(ctx, time.Since(connectStart).Seconds(),
		metric.WithAttributes(
			attribute.String("server", srv.Name),
			attribute.String("outcome", connectOutcome),
		),
	)
	if err != nil {
		return dialOutcome{cancel: cancel, watchdog: watchdog, reason: "connect", err: err}
	}
	return dialOutcome{conn: conn, upCtx: upCtx, cancel: cancel, watchdog: watchdog}
}

func (s *Server) skipUpstream(ctx context.Context, sess *session, serverID string, watchdog *time.Timer, cancel context.CancelFunc, conn *upstreamConn, reason string, err error) {
	watchdog.Stop()
	if conn != nil {
		_ = conn.session.Close()
		if conn.transport != nil {
			conn.transport.CloseIdleConnections()
		}
	}
	cancel()
	attrs := []any{"server_id", serverID, "reason", reason}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
	}
	s.logger.Warn("skip upstream", attrs...)
	s.audit(ctx, sess, "gateway.upstream.connect", serverID, "error")
}

// audit best-effort-writes a gateway audit event (spec §9: best-effort with
// error-logging; a per-call DENY returns an error to the caller regardless of
// the audit write, so a dropped audit cannot make a denied call look allowed).
//
// It does NOT touch the RBACDecision metric (audit G12): that counter must
// reflect only the per-call tool allow/deny decisions made by rbacMiddleware,
// not every audit event this method writes (e.g. gateway.upstream.connect
// during session build) -- see rbacMiddleware's auditDecision helper.
//
// metadata is optional (variadic so every existing call site above, all
// fixed-arity, needed no edit) -- at most its first map is attached to the
// stored audit row's Metadata column. intercept.go's auditCallFinding is the
// first caller to pass one, carrying a finding's rule+severity (docs/specs/
// 2026-08-25-orbeat-runtime-interception-design.md §5: NEVER the scanned
// content) rather than duplicating this method's best-effort/dual-emit
// behaviour in a second near-identical helper.
func (s *Server) audit(ctx context.Context, sess *session, action, target, decision string, metadata ...map[string]any) {
	s.auditAs(ctx, sess.tenantID, sess.actor, action, target, decision, metadata...)
}

// auditAs is audit's body with the tenant and actor passed in rather than
// read off a *session. It exists for the one caller that has to write an
// audit record BEFORE a session exists and will never build one:
// resolveVirtualKey's refusal of an orphaned service account
// (virtualkey.ee.go), which knows the tenant (it resolved it in order to look
// the key up) and the actor (the token's subject) but by construction has no
// session, since refusing to build one is the point.
//
// Factored out rather than written a second time so the best-effort write,
// the SIEM dual-emit and the audit_db_write=failed flag stay in one place.
// The alternative, a caller assembling its own store.AuditEvent and calling
// AppendAuditEvent directly, would have been a second copy of that behaviour
// that drifts the first time either half changes -- and the half that matters
// is the one that still emits to the log when the DB write fails, which is
// exactly the half a hand-rolled call site forgets.
func (s *Server) auditAs(ctx context.Context, tenantID, actor, action, target, decision string, metadata ...map[string]any) {
	ev := store.AuditEvent{
		TenantID: tenantID, Actor: actor, Action: action,
		Target: target, Decision: decision,
	}
	if len(metadata) > 0 {
		ev.Metadata = metadata[0]
	}
	_, err := s.store.AppendAuditEvent(ctx, ev)
	attrs := []any{
		"event", "audit",
		"actor", actor,
		"action", action,
		"target", target,
		"decision", decision,
		"tenant", tenantID,
		"request_id", requestIDFor(ctx),
	}
	if len(metadata) > 0 {
		attrs = append(attrs, "metadata", metadata[0])
	}
	if err != nil {
		// Best-effort DB write failed -- still emit to the log stream so the SIEM
		// records the decision, flagged distinguishable from a durable audit row.
		s.logger.Warn("audit", append(attrs, "audit_db_write", "failed", "err", err.Error())...)
		return
	}
	s.logger.Info("audit", attrs...)
}
