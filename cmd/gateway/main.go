// Command orbeat-gateway serves the orbeat MCP gateway broker.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/config"
	"github.com/stefanocalabrese/orbeat-community/internal/gateway"
	"github.com/stefanocalabrese/orbeat-community/internal/govern"
	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/ratelimit"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
	"github.com/stefanocalabrese/orbeat-community/internal/version"
)

const (
	// rateLimitTTL bounds a rate-limit key's idle lifetime before eviction
	// (design spec §3.1 invariant 2: ttl must be >= burst/rps for every
	// configured budget — at the lowest default rps (the initialize budget,
	// 1/s) and the shared default burst (60), burst/rps = 60s, comfortably
	// under this).
	rateLimitTTL = 10 * time.Minute
	// rateLimitMaxEntries bounds the key-map memory: one bucket per
	// (subject, azp) pair (spec §3.1).
	rateLimitMaxEntries = 10000
)

func main() {
	// `-healthcheck` is the container self-probe: GET our own /healthz and exit
	// 0/1. The distroless image has no shell or curl, so the compose healthcheck
	// invokes the binary itself. Handle it before any config/DB work.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthProbe(":8080"))
	}
	// run (not main) holds the body so every deferred cleanup — the telemetry
	// flush above all — executes before the process exits; os.Exit skips defers.
	os.Exit(run())
}

// healthProbe performs a GET on this service's own /healthz and returns 0 on a
// 200, 1 otherwise. It reads the listen address from ORBEAT_HTTP_ADDR (the same
// source the server uses), defaulting to defAddr; an empty/wildcard host is
// dialed as 127.0.0.1.
func healthProbe(defAddr string) int {
	addr := os.Getenv("ORBEAT_HTTP_ADDR")
	if addr == "" {
		addr = defAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: bad ORBEAT_HTTP_ADDR:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: /healthz returned", resp.StatusCode)
		return 1
	}
	return 0
}

// interceptorFor decides the runtime call-interception scanner to install
// (Task 4 of docs/plans/orbeat-runtime-interception-2026-08-25.md; design
// spec §4): scanner when cfg.Intercept is set, nil when it is unset (the
// default). Returning nil rather than a scanner that finds nothing is the
// point -- internal/gateway's interceptArguments/interceptResult (intercept.go)
// both short-circuit on a nil interceptor before touching the store or the
// scanner at all, so "off" here means the hook is never installed, not
// installed-and-inert.
//
// Factored out of run() as a pure function (govern.Scanner in, govern.Scanner
// out, no *gateway.Server) so the off path is unit-testable without dialing a
// database: run() itself cannot be exercised end to end in this package (it
// connects to cfg.DBURL before reaching gw.SetInterceptor), the same
// constraint cmd/api/dcr_client_wiring_test.go's TestRunWiresDCRClient notes
// for its own equivalent call.
func interceptorFor(cfg config.Config, scanner govern.Scanner) govern.Scanner {
	if cfg.Intercept == "" {
		return nil
	}
	return scanner
}

func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		return 1
	}
	if err := cfg.RequireOIDC(); err != nil {
		slog.Error("config", "err", err)
		return 1
	}

	slog.SetDefault(logging.New(os.Stdout, cfg.LogFormat, cfg.LogLevel).With("service", "orbeat-gateway"))

	fatal := func(msg string, err error) int {
		slog.Error(msg, "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tel, shutdownTel, err := telemetry.Setup(ctx, telemetry.Config{
		Endpoint:    cfg.OTelEndpoint,
		ServiceName: orDefault(cfg.OTelServiceName, "orbeat-gateway"),
		// version.Version, not a literal. This was hardcoded "dev" until audit
		// C8, while release.yml injects the real tag into internal/version via
		// -ldflags, so every trace and metric emitted by a signed release image
		// was attributed to "dev" and no telemetry could be tied to a release.
		// internal/version's own doc says it "must be the ONLY place the
		// version is read from" and names three consumers; this was a silent
		// fourth. Gated by TestOTelServiceVersionIsNotALiteral.
		ServiceVersion: version.Version,
		SampleRatio:    cfg.OTelSampleRatio,
		Insecure:       cfg.OTelInsecure,
	})
	if err != nil {
		return fatal("otel setup", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTel(sctx)
	}()

	st, err := store.NewWithTracer(ctx, cfg.DBURL, tel.QueryTracer())
	if err != nil {
		return fatal("store", err)
	}

	if err := telemetry.RegisterPoolGauges(tel.Meter("orbeat-gateway"),
		func() int64 {
			s := st.PoolStat()
			if s == nil {
				return 0
			}
			return int64(s.AcquiredConns())
		},
		func() int64 {
			s := st.PoolStat()
			if s == nil {
				return 0
			}
			return int64(s.IdleConns())
		},
	); err != nil {
		slog.Warn("otel pool gauges", "err", err) // non-fatal
	}

	// context.Background(), deliberately NOT ctx: the JWKS cache has to outlive
	// the shutdown signal so requests still in flight during the drain can
	// validate. auth.NewValidator passes this context to jwk.NewCache, which
	// starts httprc's controller goroutine; that goroutine returns on
	// ctx.Done(), and once it is gone every jwk.Cache.Lookup sends on a channel
	// nobody receives from, so Validate blocks until the CALLER's context
	// expires and then fails. It does not fall back to cached keys. Wiring ctx
	// here therefore made every request arriving inside srv.Shutdown's 10s
	// drain below burn its whole deadline instead of validating. cmd/api's
	// run() passes context.Background() for the same reason; nothing needs to
	// cancel this context, because the refresh goroutine dies with the process.
	// Pinned by TestJWKSRefreshContextSurvivesShutdownSignal.
	v, err := auth.NewValidator(context.Background(), auth.Config{
		Issuer:       cfg.OIDCIssuer,
		Audience:     cfg.OIDCAudience,
		DiscoveryURL: cfg.OIDCDiscoveryURL,
	})
	if err != nil {
		return fatal("auth", err)
	}

	// Resolve the single tenant once at startup so the usage counter and
	// quota enforcer below can be constructed for a concrete tenant id,
	// mirroring cmd/api/main.go's identical resolve-once-at-startup step
	// for its own publish closures. GetOrCreateTenantByName is idempotent
	// (INSERT … ON CONFLICT) and safe to call before traffic.
	tn, err := st.GetOrCreateTenantByName(ctx, cfg.TenantName)
	if err != nil {
		return fatal("resolve tenant", err)
	}
	tenantID := tn.ID

	resolver := authz.NewResolver(st, cfg.TenantName)
	gw := gateway.New(st, resolver, gateway.NewTokenVerifier(v), secrets.NewResolver(), cfg.GatewayResourceURL, cfg.OIDCIssuer)
	// Gateway defaults (spec §6): 20 rps / 60 burst for tools/call, 1 rps
	// (sharing the same burst — there is no separate _INIT_BURST variable)
	// for the session-creating initialize budget (§4.3).
	gw.SetLimiter(ratelimit.New(cfg.RateLimitRPSN(20), cfg.RateLimitBurstN(60), rateLimitTTL, rateLimitMaxEntries))
	gw.SetInitLimiter(ratelimit.New(cfg.RateLimitInitRPSN(1), cfg.RateLimitBurstN(60), rateLimitTTL, rateLimitMaxEntries))
	// Per-principal concurrency cap on tools/call (fable-audit §7 #14) — a
	// different axis from the rate limiters above: it bounds how many calls
	// are in flight at once, not how fast they arrive. rateLimitTTL is reused
	// deliberately, not coincidentally: it already means "how long an idle
	// per-key entry survives before its sweeper evicts it", which is exactly
	// what ConcurrencyLimiter's ttl means for a zero-count slot (concurrency
	// design spec §6, "Zero-count entries age out on the existing TTL") — the
	// rate limiter's OTHER invariant on this constant (ttl >= burst/rps,
	// documented on rateLimitTTL below) is specific to token buckets and does
	// not apply here, but that invariant is a second, separate fact about the
	// same duration, not a redefinition of what the duration itself means.
	gw.SetInflightCap(ratelimit.NewConcurrency(cfg.GatewayMaxInflightN(8), rateLimitTTL))
	// Runtime call interception (Task 4 of docs/plans/orbeat-runtime-
	// interception-2026-08-25.md): interceptorFor returns nil unless
	// ORBEAT_INTERCEPT is set, and gw.SetInterceptor(nil) is New()'s own
	// default -- so this line changes nothing for a deployment that never
	// set the variable.
	gw.SetInterceptor(interceptorFor(cfg, govern.NewDefaultScanner()))

	// Usage metering and quota enforcement (docs/specs/2026-08-25-orbeat-
	// usage-metering-design.md; docs/plans/orbeat-usage-metering-2026-08-25.md
	// Task 5). Unlike SetInterceptor above, both are wired UNCONDITIONALLY:
	// there is no ORBEAT_USAGE_* on/off knob for the counter or the
	// enforcer themselves (config.UsageFlushIntervalDuration's doc comment
	// states why: counting is cheap and is the entire point of the
	// subsystem, and enforcement is already off in every practical sense
	// until an admin creates a role_quota row). uc/qe are both edition-safe
	// to construct and install in every build: usage.community.go and
	// quota.community.go's no-op counterparts make Count/Check inert in a
	// Community build without cmd/gateway needing its own edition branch,
	// exactly like SetInterceptor's nil-scanner pattern above.
	uc := gateway.NewUsageCounter(st, tenantID)
	qe := gateway.NewQuotaEnforcer(st, tenantID)
	gw.SetUsageCounter(uc)
	gw.SetQuotaEnforcer(qe)

	// The periodic ticker (flush + quota-cache refresh, same interval, per
	// spec section 3: "refreshed on the same interval"). Its own context
	// (not the signal ctx) so it stops in an ordered way AFTER the HTTP
	// server drains, mirroring cmd/api's retention loops and publish
	// worker; the explicit shutdown flush below is what actually persists
	// whatever this loop's own cancellation left in memory. RunUsageTicker's
	// own doc comment explains why the shutdown flush is deliberately NOT
	// inside it.
	usageCtx, cancelUsage := context.WithCancel(context.Background())
	usageDone := make(chan struct{})
	go func() {
		gateway.RunUsageTicker(usageCtx, uc.Flush, qe.RefreshAll, cfg.UsageFlushIntervalDuration())
		close(usageDone)
	}()

	// The entitlement-change nudge: a Postgres LISTEN that drops a tenant's
	// cached sessions when its entitlements, roles or servers change, so a
	// revocation lands in seconds instead of at the next five-minute rebuild.
	//
	// Its own context, like the ticker above, so it stops after the HTTP server
	// drains. It is deliberately NOT part of readiness and nothing waits for
	// it: if it never connects, the gateway behaves exactly as it did before
	// the nudge existed, which is the whole reason this is safe to start
	// fire-and-forget.
	nudgeCtx, cancelNudge := context.WithCancel(context.Background())
	nudgeDone := make(chan struct{})
	go func() {
		gw.StartEntitlementNudge(nudgeCtx)
		close(nudgeDone)
	}()

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: gw.Handler(), ReadHeaderTimeout: 5 * time.Second}

	// Serve in a goroutine; a serve failure is SENT to the main goroutine
	// rather than os.Exit'd in place — os.Exit there skipped the deferred
	// telemetry shutdown and gw.Close().
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	exit := 0
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("http shutdown", "err", err)
		}
	case err := <-serveErr:
		slog.Error("server", "err", err)
		exit = 1
	}

	// Stop the periodic ticker, THEN flush once more explicitly. Without
	// this final flush, a stopped gateway silently loses up to one flush
	// interval's worth of usage: the ticker's own last tick is not
	// synchronized with shutdown, so relying on it alone would leave
	// whatever was counted since then stranded in memory and discarded when
	// the process exits. A fresh context (not shutdownCtx above, which may
	// already be near its deadline by the time execution reaches here, and
	// not the signal ctx, which is already Done) bounds this last write.
	cancelNudge()
	<-nudgeDone
	cancelUsage()
	<-usageDone
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 10*time.Second)
	if err := uc.Flush(flushCtx); err != nil {
		slog.Error("usage flush on shutdown", "err", err)
	}
	cancelFlush()

	gw.Close() // drain cached upstream MCP connections
	return exit
}

// orDefault returns s, or def if s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
