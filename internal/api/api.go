package api

import (
	_ "embed"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/govern"
	"github.com/stefanocalabrese/orbeat-community/internal/httpx"
	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/publish"
	"github.com/stefanocalabrese/orbeat-community/internal/ratelimit"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
)

// openapiSpec is the API's OpenAPI 3.0.3 document, served unauthenticated at
// GET /openapi.yaml. Embedded so the binary is self-describing with no
// external file dependency at runtime.
//
//go:embed openapi.yaml
var openapiSpec []byte

// nopEnqueuer is a no-op Enqueuer used when publishing is disabled or in tests.
type nopEnqueuer struct{}

func (nopEnqueuer) Enqueue() {}

// Server holds the API's dependencies.
type Server struct {
	store       *store.Store
	resolver    *authz.Resolver
	validator   *auth.Validator
	corsOrigins []string // exact-match allow-list; nil → fail closed
	publisher   publish.Enqueuer
	scanner     govern.Scanner
	secrets     *secrets.Resolver
	logger      *slog.Logger
	metrics     *telemetry.Metrics
	gatewayURL  string // advertised to the sync client via /v1/sync/config; set by cmd/api

	// auditExportMaxRows caps GET /v1/admin/audit/export; 0 = unlimited (the
	// export already streams row-by-row, so unlimited is memory-safe). Default
	// 100000, set by New; overridden by cmd/api from ORBEAT_AUDIT_EXPORT_MAX_ROWS.
	auditExportMaxRows int

	// rateLimit is nil by default (New never sets it), meaning "no limiting" —
	// every test that builds a Server directly (most of this package) is
	// unaffected. cmd/api wires a real *ratelimit.Limiter via SetRateLimiter.
	rateLimit *ratelimit.Limiter

	// revisionKeep caps how many artifact_revision rows SetArtifactApproved/
	// RollbackArtifact retain per artifact after each write (0 = unlimited, no
	// pruning — New's zero value, so existing deployments are unchanged).
	// cmd/api wires it from cfg.ArtifactRevisionKeepN()
	// (ORBEAT_ARTIFACT_REVISION_KEEP). Deliberately a plain int passed down
	// into the store calls, never Store state (spec
	// docs/specs/2026-08-19-orbeat-revision-pruning-design.md §4): InTx builds
	// the transaction-bound Store as a fresh &Store{db: pgtx}, carrying only
	// db, so a field on Store would silently be zero inside every transaction
	// insertRevision actually runs in.
	revisionKeep int

	// limits are this edition's write-time caps (docs/specs/2026-08-19-
	// orbeat-community-caps-design.md §4), set once by New from
	// communityLimits() (limits.ee.go / limits.community.go). Zero fields
	// mean unlimited, matching revisionKeep/auditExportMaxRows' convention.
	// Unexported: nothing outside a test in this package ever needs to
	// change it, since the edition IS the build, not a runtime choice.
	limits editionLimits

	// contactEmail is the address a 402 cap response points an admin at.
	// Defaults to authz.DefaultContactEmail, set by New; cmd/api wires
	// ORBEAT_CONTACT_EMAIL to override it via SetContactEmail.
	contactEmail string

	// autoApprove reports whether handleCreateArtifact/handleUpdateArtifact
	// immediately approve the working copy they just wrote (docs/specs/
	// 2026-08-19-orbeat-community-caps-design.md §2), set once by New from
	// communityAutoApprove() (autoapprove.ee.go / autoapprove.community.go,
	// the same shape as limits/communityLimits above). False in this repo's
	// own Enterprise build, where only the real approval workflow
	// (admin_artifact_review.ee.go) sets approved_content; a generated
	// Community tree compiles no other value. Unexported for the same reason
	// as limits: the edition IS the build, not a runtime choice.
	autoApprove bool
}

// New builds a Server. validator may be nil in tests that call handlers
// directly. corsOrigins is the browser cross-origin allow-list; nil disables
// CORS headers entirely (fail closed). pub may be nil (publishing disabled).
func New(s *store.Store, r *authz.Resolver, v *auth.Validator, corsOrigins []string, pub publish.Enqueuer) *Server {
	if pub == nil {
		pub = nopEnqueuer{}
	}
	return &Server{
		store: s, resolver: r, validator: v, corsOrigins: corsOrigins, publisher: pub,
		scanner:            govern.NewDefaultScanner(),
		secrets:            secrets.NewResolver(),
		logger:             slog.Default(),
		metrics:            telemetry.NewMetrics(otel.Meter("orbeat-api")),
		auditExportMaxRows: 100000,
		limits:             communityLimits(),
		contactEmail:       authz.DefaultContactEmail,
		autoApprove:        communityAutoApprove(),
	}
}

// SetScanner overrides the artifact governance scanner (default: rule-based).
// cmd/api wires a CompositeScanner (rules + LLM) here when the LLM endpoint is
// configured, otherwise the rule scanner. A nil argument
// is ignored so the default is never accidentally wiped.
func (s *Server) SetScanner(sc govern.Scanner) {
	if sc != nil {
		s.scanner = sc
	}
}

// SetSecrets overrides the secrets resolver used to validate secret refs
// (mirroring SetScanner). New already installs the full default resolver, so no
// caller needs this today; it exists so a deployment registering a narrower or
// wider provider set can swap one in without changing New's signature.
//
// A nil argument is IGNORED rather than stored. That is the load-bearing half:
// it is what keeps New's non-nil default from being defeated, so ref validation
// cannot be silently turned off. TestSetSecretsOverridesAndIgnoresNil pins both
// halves.
func (s *Server) SetSecrets(r *secrets.Resolver) {
	if r != nil {
		s.secrets = r
	}
}

// SetGatewayURL sets the gateway resource URL advertised to the sync client on
// GET /v1/sync/config. cmd/api wires it from cfg.GatewayResourceURL.
func (s *Server) SetGatewayURL(u string) { s.gatewayURL = u }

// SetAuditExportMaxRows overrides the audit export row cap (default 100000,
// set by New). 0 means unlimited: handleExportAudit skips the truncation
// check entirely and streams every matching row. cmd/api wires it from
// cfg.AuditExportMaxRowsN() (ORBEAT_AUDIT_EXPORT_MAX_ROWS).
func (s *Server) SetAuditExportMaxRows(n int) { s.auditExportMaxRows = n }

// SetRateLimiter installs the per-principal token-bucket limiter (spec
// docs/specs/2026-08-12-orbeat-rate-limiting-design.md §4.1). Not called by
// New: New has no way to know a caller's chosen rps/burst/ttl/maxEntries, and
// New's own doc comment says validator may be nil for tests calling handlers
// directly — those tests must stay unaffected. cmd/api wires this from
// cfg.RateLimitRPSN/RateLimitBurstN. A nil argument is a no-op (mirrors
// SetSecrets/SetScanner's nil-ignore contract) so a caller can never
// accidentally disable limiting by passing a stray nil.
func (s *Server) SetRateLimiter(l *ratelimit.Limiter) {
	if l != nil {
		s.rateLimit = l
	}
}

// SetArtifactRevisionKeep installs the artifact_revision prune cap (default 0
// = unlimited, New's zero value). Unconditional, unlike SetScanner/
// SetSecrets/SetRateLimiter's nil-ignore contract — those guard a pointer
// default against being wiped by a stray nil, but 0 has no such competing
// meaning here: it IS the documented "off" default (mirrors
// SetAuditExportMaxRows, whose own 0 means unlimited exports). cmd/api wires
// this from cfg.ArtifactRevisionKeepN() (ORBEAT_ARTIFACT_REVISION_KEEP).
func (s *Server) SetArtifactRevisionKeep(n int) { s.revisionKeep = n }

// SetContactEmail overrides the 402 cap response's contact address (default
// authz.DefaultContactEmail, set by New). Empty is ignored, mirroring
// SetScanner/SetSecrets/SetRateLimiter's nil-ignore contract, so a caller can
// never accidentally blank the default. cmd/api wires this from
// cfg.ContactEmail (ORBEAT_CONTACT_EMAIL).
func (s *Server) SetContactEmail(email string) {
	if email != "" {
		s.contactEmail = email
	}
}

// rateLimited wraps next with the rate limiter when one is installed, and is
// a pass-through otherwise — s.rateLimit is nil unless SetRateLimiter was
// called, which is exactly the "no limiting" contract every direct-construction
// test (most of this package) relies on. ratelimit.HTTP itself has no nil-safe
// mode (its Limiter methods would nil-deref), so the nil check has to live
// here rather than there.
func (s *Server) rateLimited(next http.Handler) http.Handler {
	if s.rateLimit == nil {
		return next
	}
	return ratelimit.HTTP(s.rateLimit, denyRateLimited, ratelimit.Observability{Metrics: s.metrics, Logger: s.logger}, next)
}

// denyRateLimited writes the 429 in the repo's standard error envelope.
// ratelimit.HTTP has already set the Retry-After header before calling this.
func denyRateLimited(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
}

// maxRequestBodyBytes caps a mutating request's body (audit B3: "no request-
// body size limit anywhere" — an admin could POST multi-GB artifact content,
// stored verbatim into every revision, audit export, and both distribution
// channels; a slow-dripped body also held a connection open forever). 1 MiB
// comfortably covers the largest legitimate payload (an artifact's content,
// hard-capped at 64KiB in validateArtifact) with headroom for JSON overhead.
const maxRequestBodyBytes = 1 << 20

// maxBytesMiddleware wraps POST/PUT bodies in http.MaxBytesReader, closing the
// unbounded-body vector at the transport boundary — the actual enforcement is
// on every Read from r.Body, so it protects both a huge single payload and a
// slow-dripped one. GET/DELETE carry no body in this API, so they're left
// untouched. decodeJSONOrFail (admin_servers.go) maps the resulting
// *http.MaxBytesError to 413.
func maxBytesMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// Handler returns the fully-wired HTTP handler (routes + middleware).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpx.HealthHandler())
	mux.Handle("GET /openapi.yaml", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(currentOpenAPISpec())
	}))

	// Authenticated + resolved identity (DB tenant/user/roles) for app routes.
	// rateLimited sits AFTER RequireAuth (it needs a principal) and BEFORE
	// resolver.Middleware, so a rejected request never pays the tenant/user
	// upsert (spec §4.1) — the same reasoning that puts RequireRole before the
	// resolver below.
	authed := func(h http.HandlerFunc) http.Handler {
		return s.validator.RequireAuth(s.rateLimited(s.resolver.Middleware(recordIdentity(h))))
	}
	// Admin: authenticated + must carry the orbeat-admin realm role + resolved.
	// RequireRole runs BEFORE resolver.Middleware (audit B4): it decides purely
	// from the token's realm roles (see authz.RequireRole), so a non-admin's
	// 403 no longer pays for the tenant/user upsert + role reconcile — those
	// three queries now only run for requests that pass the role gate. Handlers
	// still see a fully resolved context (via s.resolved), since
	// resolver.Middleware directly wraps the handler in this order too.
	// rateLimited sits between RequireRole and the resolver for the same
	// before-the-DB reason as the authed closure above.
	admin := func(h http.HandlerFunc) http.Handler {
		return s.validator.RequireAuth(
			authz.RequireRole("orbeat-admin")(s.rateLimited(s.resolver.Middleware(recordIdentity(http.HandlerFunc(h))))))
	}

	// /v1/me sits outside both closures above (it does no DB resolve at all),
	// so it must be rate-limited explicitly here or it ships unlimited.
	mux.Handle("GET /v1/me", s.validator.RequireAuth(s.rateLimited(recordIdentity(http.HandlerFunc(s.handleMe)))))
	mux.Handle("GET /v1/catalog", authed(s.handleCatalog))
	mux.Handle("GET /v1/sync/artifacts", authed(s.handleSyncArtifacts))
	mux.Handle("GET /v1/sync/config", authed(s.handleSyncConfig))

	mux.Handle("POST /v1/admin/servers", admin(s.handleCreateServer))
	mux.Handle("GET /v1/admin/servers", admin(s.handleListServers))
	mux.Handle("GET /v1/admin/servers/{id}", admin(s.handleGetServer))
	mux.Handle("PUT /v1/admin/servers/{id}", admin(s.handleUpdateServer))
	mux.Handle("DELETE /v1/admin/servers/{id}", admin(s.handleDeleteServer))

	mux.Handle("POST /v1/admin/roles", admin(s.handleCreateRole))
	mux.Handle("GET /v1/admin/roles", admin(s.handleListRoles))
	mux.Handle("DELETE /v1/admin/roles/{id}", admin(s.handleDeleteRole))

	mux.Handle("DELETE /v1/admin/users/{id}", admin(s.handleDeleteUser))

	mux.Handle("POST /v1/admin/entitlements", admin(s.handleCreateEntitlement))
	mux.Handle("GET /v1/admin/entitlements", admin(s.handleListEntitlements))
	mux.Handle("DELETE /v1/admin/entitlements/{id}", admin(s.handleDeleteEntitlement))

	mux.Handle("GET /v1/admin/audit", admin(s.handleListAudit))

	mux.Handle("POST /v1/admin/artifacts", admin(s.handleCreateArtifact))
	mux.Handle("GET /v1/admin/artifacts", admin(s.handleListArtifacts))
	mux.Handle("GET /v1/admin/artifacts/{id}", admin(s.handleGetArtifact))
	mux.Handle("PUT /v1/admin/artifacts/{id}", admin(s.handleUpdateArtifact))
	mux.Handle("DELETE /v1/admin/artifacts/{id}", admin(s.handleDeleteArtifact))
	mux.Handle("POST /v1/admin/marketplace/publish", admin(s.handleMarketplacePublish))
	mux.Handle("GET /v1/admin/marketplace/status", admin(s.handleMarketplaceStatus))
	mux.Handle("POST /v1/admin/artifact-entitlements", admin(s.handleCreateArtifactEntitlement))
	mux.Handle("GET /v1/admin/artifact-entitlements", admin(s.handleListArtifactEntitlements))
	mux.Handle("DELETE /v1/admin/artifact-entitlements/{id}", admin(s.handleDeleteArtifactEntitlement))

	// The artifact review lifecycle (submit/approve/reject/withdraw), revision
	// history + rollback, and audit export are Enterprise-only. Kept out of
	// this file so a Community build (open-core generation) can drop the file
	// that defines them without api.go naming a single Enterprise handler.
	s.registerEnterpriseRoutes(mux, admin)

	return otelhttp.NewHandler(
		withIdentityCarrier(logging.Requests(s.logger, apiIdentity)(corsMiddleware(s.corsOrigins)(maxBytesMiddleware(mux)))),
		"http.server",
	)
}
