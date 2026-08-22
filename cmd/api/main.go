// Command orbeat-api serves the orbeat control-plane API.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/stefanocalabrese/orbeat-community/internal/api"
	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/config"
	"github.com/stefanocalabrese/orbeat-community/internal/govern"
	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/marketplace"
	"github.com/stefanocalabrese/orbeat-community/internal/migrate"
	"github.com/stefanocalabrese/orbeat-community/internal/publish"
	"github.com/stefanocalabrese/orbeat-community/internal/ratelimit"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
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

	slog.SetDefault(logging.New(os.Stdout, cfg.LogFormat, cfg.LogLevel).With("service", "orbeat-api"))

	fatal := func(msg string, err error) int {
		slog.Error(msg, "err", err)
		return 1
	}

	// Built once and threaded through every call site below: VaultProvider/
	// AWSSMProvider cache their backend client per instance (sync.Once), so
	// reusing one Resolver avoids each site building and authenticating its own
	// client against the same backend.
	secretsResolver := secrets.NewResolver()

	// Fail closed at boot on a malformed marketplace git credential ref. Without
	// this, the ref is only exercised at first push, so a typo stays invisible
	// until someone publishes.
	//
	// Scheme+shape only, deliberately NOT Resolve: resolving here would require
	// Vault/AWS to be reachable before orbeat-api can start, breaking a legitimate
	// lazy secrets setup. ValidateRef is I/O-free by construction, and it already
	// treats an empty ref as valid ("no credential"), which is what an unset
	// ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF means. (ORBEAT_SCAN_LLM_KEY_REF does
	// fully resolve at boot; that asymmetry is intended — see the design spec §1.)
	//
	// The message below names the env var (unlike its lowercase-action-phrase
	// neighbors, e.g. "otel setup"/"resolve tenant") deliberately: it is the
	// exact string an operator would grep logs/docs for.
	if err := secretsResolver.ValidateRef(cfg.MarketplaceGitCredentialRef); err != nil {
		return fatal("validate ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF", err)
	}

	// ctx ends on SIGINT/SIGTERM (every `docker compose down`, every pod
	// eviction) and drives the graceful-shutdown path below.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tel, shutdownTel, err := telemetry.Setup(ctx, telemetry.Config{
		Endpoint:       cfg.OTelEndpoint,
		ServiceName:    orDefault(cfg.OTelServiceName, "orbeat-api"),
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

	// Run migrations before opening the pool.
	db, err := sql.Open("pgx", cfg.DBURL)
	if err != nil {
		return fatal("open db for migrate", err)
	}
	if err := migrate.Up(db); err != nil {
		return fatal("migrate", err)
	}
	_ = db.Close()

	st, err := store.NewWithTracer(ctx, cfg.DBURL, tel.QueryTracer())
	if err != nil {
		return fatal("store", err)
	}

	if err := telemetry.RegisterPoolGauges(tel.Meter("orbeat-api"),
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

	v, err := auth.NewValidator(context.Background(), auth.Config{
		Issuer:       cfg.OIDCIssuer,
		Audience:     cfg.OIDCAudience,
		DiscoveryURL: cfg.OIDCDiscoveryURL,
	})
	if err != nil {
		return fatal("auth", err)
	}

	resolver := authz.NewResolver(st, cfg.TenantName)

	// Resolve the single tenant once at startup so the publish closures can
	// capture its ID without a per-call DB round-trip. GetOrCreateTenantByName
	// is idempotent (INSERT … ON CONFLICT) and safe to call before traffic.
	tn, err := st.GetOrCreateTenantByName(context.Background(), cfg.TenantName)
	if err != nil {
		return fatal("resolve tenant", err)
	}
	tenantID := tn.ID

	var pub publish.Enqueuer
	// stopWorker cancels the publish worker's context and waits for Start to
	// return; nil when publishing is disabled. Run it AFTER the HTTP server has
	// drained so a request-triggered Enqueue near shutdown still lands.
	var stopWorker func()
	if cfg.MarketplaceGitURL != "" {
		gitTimeout, ok := cfg.MarketplaceGitTimeoutDuration()
		if !ok {
			slog.Warn("invalid ORBEAT_MARKETPLACE_GIT_TIMEOUT; using 120s", "value", cfg.MarketplaceGitTimeout)
		}
		publisher := publish.New(publish.Config{
			GitURL:        cfg.MarketplaceGitURL,
			CredentialRef: cfg.MarketplaceGitCredentialRef,
			GatewayURL:    cfg.GatewayResourceURL,
			Timeout:       gitTimeout,
		}, secretsResolver, func(ctx context.Context) ([]marketplace.Artifact, error) {
			return publish.ActiveArtifacts(ctx, st, tenantID)
		})
		worker := publish.NewWorker(publisher, 750*time.Millisecond, func(ctx context.Context, res publish.Result, err error) {
			if err != nil {
				slog.Error("marketplace publish failed", "err", err)
			}
			publish.RecordResult(ctx, st, tenantID, res, err)
		})
		// The worker gets its own context (NOT the signal ctx) so shutdown is
		// ordered: HTTP drains first, then the worker is stopped explicitly and
		// awaited — a SIGTERM must not kill a publish mid-git-push.
		workerCtx, cancelWorker := context.WithCancel(context.Background())
		workerDone := make(chan struct{})
		go func() {
			worker.Start(workerCtx)
			close(workerDone)
		}()
		stopWorker = func() {
			cancelWorker()
			<-workerDone
		}
		worker.Enqueue() // publish once on startup to sync catalog state
		pub = worker
	}

	// Audit retention (off by default): prune old audit_event rows on a ticker.
	// Its own context (not the signal ctx) so retention is stopped in an ordered
	// way AFTER the HTTP server drains — mirroring the publish worker. An in-flight
	// prune batch is cancelled cleanly (a batched DELETE rolls back atomically and
	// resumes next interval), not killed mid-teardown.
	var stopRetention func()
	if days := cfg.AuditRetentionDaysN(); days > 0 {
		retCtx, cancelRet := context.WithCancel(context.Background())
		retDone := make(chan struct{})
		go func() {
			api.RunAuditRetention(retCtx, st.PruneAuditEventsOlderThan, days, cfg.AuditRetentionIntervalDuration())
			close(retDone)
		}()
		stopRetention = func() { cancelRet(); <-retDone }
		slog.Info("audit retention enabled", "older_than_days", days, "interval", cfg.AuditRetentionIntervalDuration().String())
	}

	srv := api.New(st, resolver, v, cfg.CORSOrigins, pub)

	// Governance scanner: rules always; buildScanner (scanner_enterprise.ee.go /
	// scanner_enterprise.community.go) additionally wraps in an LLM semantic
	// scanner when configured — Enterprise-only, kept out of this shared file
	// (docs/specs/2026-08-19-orbeat-community-repo-generation-design.md §4).
	scanner, err := buildScanner(ctx, cfg, secretsResolver, govern.NewDefaultScanner())
	if err != nil {
		return fatal("build scanner", err)
	}
	srv.SetScanner(scanner)
	srv.SetGatewayURL(cfg.GatewayResourceURL)
	srv.SetAuditExportMaxRows(cfg.AuditExportMaxRowsN())

	// Artifact revision pruning cap (0 = unlimited, no pruning — the default,
	// spec docs/specs/2026-08-19-orbeat-revision-pruning-design.md §5).
	// keep is unconditionally wired: ArtifactRevisionKeepN already collapses
	// unset/zero/negative/unparseable to 0, which SetArtifactRevisionKeep
	// treats as "off", mirroring SetAuditExportMaxRows just above.
	keep := cfg.ArtifactRevisionKeepN()
	srv.SetArtifactRevisionKeep(keep)
	// keep=1 and keep=2 are ACCEPTED, not rejected (config.ArtifactRevisionKeepN
	// never errors — Load() is also called by cmd/gateway, which never reads
	// this knob, so a strict-fail here would stop the gateway starting over a
	// value it does not use). Both leave a rollback-hostile shape worth
	// naming at startup rather than discovering it as a 404/409 in the field:
	//   - keep=1: insertRevision prunes every row but the one just written, so
	//     no revision other than the current one survives to be a rollback
	//     target. POST .../rollback can only ever 404.
	//   - keep=2: the surviving set after each approve/rollback is exactly
	//     {max-1, max}. max is current, so rolling back to it is the
	//     ErrRollbackNoop 409; max-1 is the one usable target, and rolling
	//     back to it appends a new revision whose own insert-then-prune
	//     immediately removes max-1 again (only the newest 2 of the resulting
	//     3 survive) — so using the one usable step spends it.
	if keep == 1 || keep == 2 {
		slog.Warn("ORBEAT_ARTIFACT_REVISION_KEEP leaves little to roll back to",
			"keep", keep,
			"consequence", map[int]string{
				1: "no revision but the current one ever survives a prune; POST .../rollback can only 404",
				2: "exactly one usable rollback step survives; rolling back to it prunes that step away",
			}[keep])
	}

	// Community 402 cap-response contact address (ORBEAT_CONTACT_EMAIL).
	// cfg.ContactEmail is empty unless the operator set it; both
	// SetContactEmail methods ignore an empty string and keep their own
	// authz.DefaultContactEmail default, so passing it straight through on
	// every boot is correct whether or not the operator configured one.
	// Wired to BOTH resolver (the seat cap's 402, written by
	// internal/authz) and srv (the server/role caps' 402, written by
	// internal/api): the three caps are enforced in two different packages
	// that each build their own 402 body, so setting only one would leave
	// one cap silently pointed at the default while the other two honour
	// the operator's override.
	resolver.SetContactEmail(cfg.ContactEmail)
	srv.SetContactEmail(cfg.ContactEmail)

	// Per-principal rate limiting (spec docs/specs/2026-08-12-orbeat-rate-limiting-design.md).
	// 50 rps / burst 200 is this binary's default (§6); ttl=5m comfortably
	// clears the §3.1 invariant 2 floor (ttl >= burst/rps, 4s at the default)
	// with the same order of magnitude as the gateway's own sessionMaxAge.
	// maxEntries bounds the key map's memory regardless of how many distinct
	// (subject, azp) pairs show up.
	srv.SetRateLimiter(ratelimit.New(cfg.RateLimitRPSN(50), cfg.RateLimitBurstN(200), 5*time.Minute, 10000))

	handler := srv.Handler()

	// ReadTimeout/IdleTimeout are safe here because the API has no streaming
	// endpoints (unlike the gateway's MCP stream): a slow-loris client or a
	// leaked idle connection gets bounded instead of pinning a conn forever.
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Serve in a goroutine; a serve failure is SENT to the main goroutine
	// rather than os.Exit'd in place, so the deferred telemetry flush and the
	// worker stop below run on that path too.
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	exit := 0
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("http shutdown", "err", err)
		}
	case err := <-serveErr:
		slog.Error("server", "err", err)
		exit = 1
	}
	if stopWorker != nil {
		stopWorker()
	}
	if stopRetention != nil {
		stopRetention()
	}
	return exit
}

// orDefault returns s, or def if s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
