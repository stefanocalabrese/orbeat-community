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
	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/ratelimit"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
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
		Endpoint:       cfg.OTelEndpoint,
		ServiceName:    orDefault(cfg.OTelServiceName, "orbeat-gateway"),
		ServiceVersion: "dev",
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

	v, err := auth.NewValidator(ctx, auth.Config{
		Issuer:       cfg.OIDCIssuer,
		Audience:     cfg.OIDCAudience,
		DiscoveryURL: cfg.OIDCDiscoveryURL,
	})
	if err != nil {
		return fatal("auth", err)
	}

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
