package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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
	// transport's own SessionTimeout below — see Handler().
	sessionMaxAge = 5 * time.Minute
	// sessionTTL evicts a cached session not used for this long (idle
	// ceiling). Expressed as a multiple of sessionMaxAge rather than a
	// standalone literal, and deliberately kept > sessionMaxAge: expired()
	// (session.go) ORs an idle-past-ttl check with an age-past-maxAge check,
	// and a session's lastSeen is never before its builtAt, so the idle check
	// can only be the one that fires when ttl < maxAge. With ttl > maxAge
	// (today, and for any future maxAge under this formula) the idle rule is
	// provably inert in production — max-age is the tighter, safety-relevant
	// bound, and sessionCache's idle-ttl mechanism exists as its
	// general-purpose second eviction rule, exercised directly with a
	// tighter ttl by session_test.go. Deriving sessionTTL from sessionMaxAge
	// means raising sessionMaxAge can never silently shrink that margin and
	// activate this second, unintended rule (gateway session lifecycle
	// design, 2026-08-16 §3 — verified: the resulting 10m is numerically
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
// into a package-level constant — this constant used to be a hardcoded
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
	// tools/call (fable-audit §7 #14) — the uninstrumented latency between the
	// gateway's own http.server span (otelhttp, Handler()) and pgx's db.query
	// spans. Built once from the global provider in New() (a struct field, not
	// a bare otel.Tracer(...) call inside the hot path) so tests can inject a
	// real, recording tracer directly — mirrors telemetry.queryTracer's tr
	// field, the same DI shape this codebase already uses for span testing.
	tracer trace.Tracer

	// limiter bounds the per-principal rate of tools/call (spec §4.2); nil
	// (New's default) means limiting is disabled — every existing test that
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
	// at once — a different axis from limiter, which bounds calls per second.
	// A caller inside its rate budget can still hold many simultaneous long
	// calls, each pinning a goroutine and a live upstream request. nil means
	// unlimited, matching limiter's zero value.
	inflight *ratelimit.ConcurrencyLimiter
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
	}
}

// SetLimiter overrides the per-principal tools/call rate limiter (spec §4.2).
// Default nil (limiting disabled) — mirrors the API adapter's nil-safe
// Limiter (rate-limiting plan Task 4), so no existing test constructing a
// Server directly needs to change. cmd/gateway wires this from
// ORBEAT_RATELIMIT_RPS/ORBEAT_RATELIMIT_BURST.
func (s *Server) SetLimiter(l *ratelimit.Limiter) { s.limiter = l }

// SetInitLimiter overrides the separate, typically-lower-budget limiter
// bounding the RATE of session-creating "initialize" calls (spec §4.3) —
// distinct from SetLimiter's tools/call budget. Default nil (unlimited).
// cmd/gateway wires this from ORBEAT_RATELIMIT_INIT_RPS.
func (s *Server) SetInitLimiter(l *ratelimit.Limiter) { s.initLimiter = l }

// SetInflightCap overrides the per-principal concurrency cap on tools/call
// (fable-audit §7 #14). Default nil (unlimited).
func (s *Server) SetInflightCap(c *ratelimit.ConcurrencyLimiter) { s.inflight = c }

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
		// in New) — reused, not a fresh literal, so the SDK's transport-session
		// idle timeout cannot silently drift from the state it fronts: our own
		// session cache already forces a full rebuild at this same age
		// regardless of activity. Without this, SessionTimeout defaulted to
		// its zero value and idle SDK transport sessions were NEVER reclaimed
		// — resident until process restart, bounded only by restarts
		// (go-sdk@v1.7.0 mcp/streamable.go:166-172, 721-725; gateway session
		// lifecycle design, 2026-08-16 §2-3). This closes the leak's symptom
		// at our seam: it proves we asked the SDK to reclaim idle transport
		// sessions, not that the SDK's process memory shrinks (§6) — that map
		// lives in a dependency this package cannot read.
		SessionTimeout: s.sessionTransportTimeout,
	})
	opts := &mcpauth.RequireBearerTokenOptions{ResourceMetadataURL: s.resource + "/.well-known/oauth-protected-resource"}
	protected := mcpauth.RequireBearerToken(s.verifier, opts)(s.withSession(mcpHandler))
	mux.Handle("/mcp", protected)
	// withIdentityCarrier must sit OUTSIDE logging.Requests (fable-audit §7
	// #14): it installs the mutable *requestIdentity that withSession fills in
	// as it learns each piece of the caller's identity, and gatewayIdentity
	// reads back — see identity.go's doc comment for why a plain non-nil
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

// withSession resolves the caller's gateway session BEFORE the SDK handler
// runs (audit G11). The SDK maps a nil *mcp.Server from getServer to
// 400 Bad Request — wrong for a server-side build failure (store/resolver
// outage): clients like Claude Code treat 400 as permanent and won't retry.
// Building here lets a failed build surface as 503 + Retry-After (a retryable
// server fault) while getServer just reads the prebuilt session from context.
// Requests without a principal pass through session-less; getServer then
// returns nil → the SDK's 400 (the bearer middleware wrapping this one
// normally makes that case unreachable).
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromTokenInfo(mcpauth.TokenInfoFromContext(r.Context()))
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// Recorded as soon as the bearer token's Principal is recovered — true
		// even if session build below subsequently fails — so a 503 still logs
		// WHO the failing caller was (fable-audit §7 #14). No-op if ctx carries
		// no identity carrier (a caller of withSession that bypassed Handler()'s
		// withIdentityCarrier, e.g. some unit tests).
		recordSubject(r.Context(), p.Subject)
		sess, err := s.sessions.getOrBuild(p.Subject, time.Now(), func() (*session, error) {
			// WithoutCancel: the session (and its upstream SSE streams) must outlive
			// the HTTP request that happened to trigger the build — and a canceled
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
		// exists — recorded here so it's set on every successful request, not
		// only the one that happened to trigger a fresh build.
		recordTenant(r.Context(), sess.tenantID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, sess)))
	})
}

// getServer is invoked by the SDK per HTTP request; withSession has already
// built (or fetched) the caller's session. Returning nil → the SDK's 400,
// kept only for the no-principal case.
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
func (s *Server) buildSession(ctx context.Context, p auth.Principal) (*session, error) {
	// Bound ONLY the DB-resolution phase. ctx itself must stay deadline-free:
	// each upstream's upCtx derives from it, and a deadline there would sever
	// live SSE streams when it fired.
	dbCtx, dbCancel := context.WithTimeout(ctx, sessionBuildDBTimeout)
	defer dbCancel()
	rc, err := s.resolver.Resolve(dbCtx, p)
	if err != nil {
		return nil, err
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
		mcpServer: mcp.NewServer(gatewayImplementation(), nil),
	}

	for _, srv := range servers {
		if _, ok := visible[srv.ID]; !ok {
			continue
		}
		// Only "active" servers are live: skip non-active (draft/disabled/…) entries
		// even when entitled. This is an intentional skip, not a connect failure, so
		// it is NOT audited as an upstream-connect error — silently continue, like the
		// not-visible case above.
		if srv.Status != "active" {
			continue
		}
		if srv.Transport != "http" && srv.Transport != "sse" {
			s.logger.Warn("skip upstream", "server_id", srv.ID, "reason", "unsupported transport", "transport", srv.Transport)
			s.audit(ctx, sess, "gateway.upstream.connect", srv.ID, "error")
			continue
		}
		// Slug-collision guard (audit G3): naming.Slugify is lossy, so distinct
		// server names can share a slug. Without this check the last writer would
		// silently overwrite slugToServer while the earlier server's tools stayed
		// registered — per-call RBAC would then authorize those tools against the
		// LATER server's entitlements. The first server to connect successfully
		// wins (stable ORDER BY name makes that deterministic when both would
		// connect; a dead first collider correctly cedes the slug); the collider is
		// skipped BEFORE dialing, so its conn is never opened and its tools can
		// never clobber the winner's in the SDK registry (which silently replaces
		// same-named tools). The api rejects colliding names at create/update,
		// but that check is advisory-pre-tx — this guard is the backstop.
		if winnerID, taken := sess.slugToServer[naming.Slugify(srv.Name)]; taken {
			s.logger.Error("slug collision",
				"server_id", srv.ID, "name", srv.Name,
				"slug", naming.Slugify(srv.Name), "winner_server_id", winnerID)
			s.audit(ctx, sess, "gateway.upstream.connect", srv.ID, "error")
			continue
		}
		secret, err := s.secrets.Resolve(ctx, srv.SecretRef)
		if err != nil {
			// err is a ref/scheme/"is not set" error, never the secret value.
			s.logger.Warn("skip upstream", "server_id", srv.ID, "reason", "secret resolve", "err", err.Error())
			s.audit(ctx, sess, "gateway.upstream.connect", srv.ID, "error")
			continue
		}
		var caPEM string
		if srv.TLSCARef != "" {
			caPEM, err = s.secrets.Resolve(ctx, srv.TLSCARef)
			if err != nil {
				// err is a ref/scheme error, never the resolved bytes.
				s.logger.Warn("skip upstream", "server_id", srv.ID, "reason", "tls ca resolve", "err", err.Error())
				s.audit(ctx, sess, "gateway.upstream.connect", srv.ID, "error")
				continue
			}
		}
		// Per-upstream cancellable ctx with a cancel-on-failure watchdog: the ctx
		// must outlive the dial (the SSE hanging GET is bound to it), so a plain
		// WithTimeout would sever the live stream when its timer fired. The
		// watchdog cancels only if connect+register overruns upstreamDialTimeout;
		// on success it is disarmed and cancel is handed to the session.
		upCtx, cancel := context.WithCancel(ctx)
		watchdog := time.AfterFunc(upstreamDialTimeout, cancel)
		connectStart := time.Now()
		conn, err := connectUpstream(upCtx, srv, secret, caPEM, s.keepAlive)
		// Measured here, not inside connectUpstream (broker.go), which is a pure
		// dial function with no metrics handle: this is DNS+TCP+TLS+handshake as
		// one number, what an operator actually pays for this upstream. Recorded
		// on BOTH outcomes — a histogram that only saw successes would hide the
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
			s.skipUpstream(ctx, sess, srv.ID, watchdog, cancel, nil, "connect", err)
			continue
		}
		if _, err := registerProxies(upCtx, sess.mcpServer, conn, sess.markDirty, s.tracer); err != nil {
			s.skipUpstream(ctx, sess, srv.ID, watchdog, cancel, conn, "register tools", err)
			continue
		}
		if !watchdog.Stop() {
			// Watchdog already fired: cancel() ran, so upCtx (and the SSE stream bound
			// to it) is severed. Don't cache a dead conn — treat as a dial-timeout failure.
			s.skipUpstream(ctx, sess, srv.ID, watchdog, cancel, conn, "dial timeout (watchdog race)", nil)
			continue
		}
		conn.cancel = cancel
		sess.slugToServer[conn.slug] = srv.ID
		sess.upstreams = append(sess.upstreams, conn)
		s.audit(ctx, sess, "gateway.upstream.connect", srv.ID, "allow")
	}

	// ONE call, rate limiter(s) before rbacMiddleware in the argument list.
	// AddReceivingMiddleware(m1, m2, m3) composes m1(m2(m3(handler))) — the
	// first argument runs outermost — but a SEPARATE later call wraps
	// whatever handler is already installed, so last-registered would run
	// first. Splitting this into two calls reverses the effective order even
	// though each call individually reads "first-argument-outermost": a
	// throttled tools/call would then reach rbacMiddleware FIRST, which
	// writes an "allow" audit row (spec §4.2) for a call that never actually
	// executes, on the governance surface this repo audits deny/allow
	// decisions on. Pinned by internal/gateway/ratelimit_test.go, not left to
	// this comment.
	rlObs := ratelimit.Observability{Metrics: s.metrics, Logger: s.logger}
	sess.mcpServer.AddReceivingMiddleware(
		ratelimit.MCP(s.limiter, "tools/call", sessionKeyFn, rlObs),
		ratelimit.MCP(s.initLimiter, "initialize", sessionKeyFn, rlObs),
		ratelimit.MCPConcurrency(s.inflight, "tools/call", sessionKeyFn, rlObs),
		s.rbacMiddleware(sess),
	)
	return sess, nil
}

// sessionKeyFn derives a ratelimit key from the PER-CALL principal carried in
// req.GetExtra().TokenInfo — see ratelimit.KeyFunc's doc for why this must
// NOT read from ctx: ctx is the jsonrpc2 connection's own context, frozen at
// whichever request established this session/connection (verified against
// the pinned go-sdk v1.7.0), so a ctx-based lookup would silently key every
// later call on this session by whichever token happened to open it,
// regardless of which client/token actually made the current call — exactly
// the outcome §6.1 exists to prevent when one gateway session (cached by
// subject alone) is shared by more than one connecting tool. ctx is accepted
// (ratelimit.KeyFunc's shape) but deliberately unused: reading
// mcpauth.TokenInfoFromContext(ctx) here instead of extra.TokenInfo is
// exactly the mistake this function exists to not make.
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
// during session build) — see rbacMiddleware's auditDecision helper.
func (s *Server) audit(ctx context.Context, sess *session, action, target, decision string) {
	_, err := s.store.AppendAuditEvent(ctx, store.AuditEvent{
		TenantID: sess.tenantID, Actor: sess.actor, Action: action,
		Target: target, Decision: decision,
	})
	attrs := []any{
		"event", "audit",
		"actor", sess.actor,
		"action", action,
		"target", target,
		"decision", decision,
		"tenant", sess.tenantID,
		"request_id", logging.RequestID(ctx),
	}
	if err != nil {
		// Best-effort DB write failed — still emit to the log stream so the SIEM
		// records the decision, flagged distinguishable from a durable audit row.
		s.logger.Warn("audit", append(attrs, "audit_db_write", "failed", "err", err.Error())...)
		return
	}
	s.logger.Info("audit", attrs...)
}
