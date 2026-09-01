package api

import (
	"context"
	_ "embed"
	"log/slog"
	"net/http"

	"github.com/lestrrat-go/jwx/v3/jwk"
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

// dcrRegisterFunc/dcrDeleteFunc are the two Keycloak operations
// admin_virtual_keys.ee.go's create/revoke handlers need
// (docs/specs/2026-08-25-orbeat-virtual-keys-design.md) -- PLAIN FUNC
// TYPES, deliberately not a named interface with methods.
//
// A first attempt used an interface, satisfied by *keycloak.DCRClient
// directly (method names RegisterClient/DeleteClient) and then, after that
// failed, by a same-named interface (Register/Delete) plus an .ee-only
// adapter. BOTH failed internal/communitygen's Enterprise-symbol boundary
// scan (TestNoSharedFileReferencesEnterpriseSymbol,
// TestGeneratedTreeContainsNoEnterpriseSymbol), which flags any SHARED
// (non-.ee.go) file spelling an identifier declared ONLY inside an .ee.go
// file, regardless of which type or interface it appears on. The second
// attempt's "generic" names did not escape this: the moment
// keycloak.VirtualKeyRegistrar declared methods named Register/Delete
// (the ONLY declaration of those exact names anywhere in the tree), THOSE
// became the new Enterprise-only symbols, and the scan then ALSO flagged an
// entirely unrelated pre-existing "Register" token in
// internal/auth/validator.go -- proving no method name, however generic,
// is safe once something adapts to it from inside the .ee boundary: Go
// interface satisfaction always requires an identically-named method on the
// concrete side, and that identical name is exactly what the scanner treats
// as the leak.
//
// A bare func value sidesteps this structurally: nothing ever has to
// DECLARE a method called "Register" or "Delete" for these fields to be
// populated. cmd/api (a future task's job; see SetDCRClient's doc comment
// below) can assign a *keycloak.DCRClient's bound methods directly --
// dcrClient.RegisterClient and dcrClient.DeleteClient are METHOD VALUES,
// not declarations -- with no new identifier declared in any shared file.
type dcrRegisterFunc func(ctx context.Context, jwks jwk.Set, name string) (clientID, registrationAccessToken string, err error)
type dcrDeleteFunc func(ctx context.Context, clientID, registrationAccessToken string) error

// roleExistsFunc is the one Keycloak operation handleUpdateRole
// (admin_roles.go, docs/plans/orbeat-role-rename-2026-08-27.md) needs:
// whether the realm named in the DCR client's issuer has a realm-level role
// named roleName. A PLAIN FUNC TYPE, for the exact reason dcrRegisterFunc/
// dcrDeleteFunc above are plain func types and not an interface satisfied
// by *keycloak.DCRClient directly — see that type's own doc comment for
// why: internal/communitygen's Enterprise-symbol boundary scan flags any
// shared (non-.ee.go) file spelling an identifier declared only inside an
// .ee.go file, and an interface method name is exactly such an identifier
// the moment something inside the .ee boundary declares a method to
// satisfy it. This file never names keycloak.DCRClient or RealmRoleExists —
// a future cmd/api wiring task assigns dcrClient.RealmRoleExists's own
// bound METHOD VALUE, declaring no new identifier here.
type roleExistsFunc func(ctx context.Context, roleName string) (bool, error)

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

	// deploymentRegistry reports whether POST /v1/sync/deployments is served
	// (docs/specs/2026-08-22-orbeat-artifact-deployment-registry-design.md
	// sec 9.2). Off by default: New leaves it false, so a deployment that
	// never sets ORBEAT_DEPLOYMENT_REGISTRY collects nothing about anybody's
	// machines.
	//
	// ONE field, read in two places, which is the point. Handler() passes it
	// to registerEnterpriseRoutes, which registers the route only when it is
	// set, and handleSyncConfig advertises the same field to the client. Two
	// fields, or a second env read, would be two expressions that happen to
	// agree until one of them is edited.
	deploymentRegistry bool

	// pinning reports whether GET /v1/sync/artifacts honours ?pin and carries
	// the three pinning fields (docs/specs/2026-08-22-orbeat-artifact-version-
	// pinning-design.md sec 6), set once by New from pinningSupported()
	// (pinning.ee.go / pinning.community.go).
	//
	// NO SETTER, unlike deploymentRegistry directly above: pinning is an
	// edition fact and nothing else, with no operator half to combine it
	// with. The one place that reading it wrong would be invisible is
	// handleSyncConfig, which is SHARED, so a Community build advertising
	// this field's value is exactly what stops it promising a client a
	// feature its own handler will not perform.
	//
	// Unexported and never poked outside a test in this package, for the same
	// reason as limits and autoApprove: the edition IS the build.
	pinning bool

	// virtualKeys reports whether POST/GET/DELETE /v1/admin/virtual-keys are
	// served here, set once by New from virtualKeysSupported()
	// (virtual_keys.ee.go / virtual_keys.community.go) -- one field, read in
	// two places, the same shape as pinning directly above: New passes it
	// (indirectly, via registerEnterpriseRoutes registering the routes only
	// in the Enterprise build) into whether the three routes exist at all,
	// and handleMe advertises the same field so a Community console never
	// renders the VirtualKeysPage a Community server would 404 on.
	//
	// NO SETTER, for the same reason as pinning: virtual keys is an edition
	// fact and nothing else (spec sec 12), with no operator half to combine
	// it with.
	virtualKeys bool

	// dcrRegister/dcrDelete register/delete Keycloak clients for virtual keys
	// (admin_virtual_keys.ee.go); see dcrRegisterFunc's doc comment above for
	// why they are plain funcs rather than an interface. Both nil by
	// default -- New never sets them, mirroring rateLimit's own
	// nil-until-wired convention -- so the create handler must treat a nil
	// dcrRegister as "not configured" and refuse cleanly rather than assume
	// cmd/api always wires one, and the revoke handler's best-effort
	// Keycloak cleanup must treat a nil dcrDelete the same way it treats any
	// other cleanup failure: log and continue.
	dcrRegister dcrRegisterFunc
	dcrDelete   dcrDeleteFunc

	// roleExists is the realm-role lookup handleUpdateRole (admin_roles.go)
	// uses to verify a rename's new name against the identity provider
	// before letting it proceed. Nil by default — New never sets it,
	// mirroring dcrRegister/dcrDelete's own nil-until-wired convention — so
	// handleUpdateRole must treat a nil roleExists as "no lookup
	// configured" (Community, or an Enterprise deployment with no
	// ORBEAT_DCR_CLIENT_ID) and require the operator's explicit assertion
	// instead. See SetRoleExistsChecker's doc comment for how a future
	// cmd/api wiring task installs a real one.
	roleExists roleExistsFunc
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
		pinning:            pinningSupported(),
		virtualKeys:        virtualKeysSupported(),
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

// SetDCRClient installs the two Keycloak operations admin_virtual_keys.ee.go's
// create/revoke handlers use to register and delete robot credentials
// (docs/specs/2026-08-25-orbeat-virtual-keys-design.md). Both are required
// together: a caller passing one nil gets NEITHER installed, so this can
// never leave the pair half-wired (a register-only or delete-only Server
// makes no sense and would be a confusing partial state to debug later).
// Mirrors SetScanner/SetSecrets/SetRateLimiter's nil-ignore contract, so a
// caller can never accidentally wipe an already-installed pair with a
// stray nil.
//
// CALLED BY cmd/api's run() (cmd/api/main.go, via cmd/api/dcr_client.ee.go's
// buildDCRClient) when ORBEAT_DCR_CLIENT_ID is configured, passing exactly
// SetDCRClient(dcrClient.RegisterClient, dcrClient.DeleteClient) --
// *keycloak.DCRClient's own bound methods as func values, per
// dcrRegisterFunc's doc comment above. Pinned by
// cmd/api.TestRunWiresDCRClient (dcr_client_wiring_test.go), an AST check on
// run() in the same spirit as TestRunWiresContactEmail and
// TestRunStartsAndStopsDeploymentRetention.
//
// UNCONFIGURED DEPLOYMENTS STAY UNAFFECTED: buildDCRClient returns
// (nil, nil, nil) when ORBEAT_DCR_CLIENT_ID is unset (the default) or in a
// Community build, so this method's nil-ignore contract above leaves
// dcrRegister/dcrDelete nil exactly as before this method had a caller, and
// POST /v1/admin/virtual-keys refuses cleanly (503, not a panic) via
// admin_virtual_keys.ee.go's existing nil-registrar branch; DELETE already
// degrades to a warning either way (spec section 8: the orbeat row is the
// source of truth regardless of whether Keycloak cleanup ever runs at all).
func (s *Server) SetDCRClient(register dcrRegisterFunc, del dcrDeleteFunc) {
	if register != nil && del != nil {
		s.dcrRegister = register
		s.dcrDelete = del
	}
}

// SetRoleExistsChecker installs the realm-role lookup handleUpdateRole
// (admin_roles.go) uses to verify a role rename's new name against the
// identity provider before letting it proceed
// (docs/plans/orbeat-role-rename-2026-08-27.md). Mirrors SetScanner/
// SetSecrets/SetRateLimiter/SetDCRClient's nil-ignore contract: a nil
// argument leaves s.roleExists at its New default (nil, "no lookup
// configured"), so handleUpdateRole falls back to requiring the operator's
// explicit assertion instead of silently disabling the guard.
//
// A future cmd/api wiring task calls this when ORBEAT_DCR_CLIENT_ID is
// configured — the SAME "orbeat-dcr" service account SetDCRClient already
// wires, per the design's decision to extend that one credential with
// realm-management's view-realm role rather than introduce a second
// service account — passing dcrClient.RealmRoleExists, again a bound
// METHOD VALUE declaring no new identifier in this shared file (see
// roleExistsFunc's doc comment above).
func (s *Server) SetRoleExistsChecker(fn roleExistsFunc) {
	if fn != nil {
		s.roleExists = fn
	}
}

// SetDeploymentRegistry turns the artifact deployment registry on or off and
// returns what was actually stored, which is what the caller should log.
//
// The stored value is `on && deploymentRegistrySupported()`. That conjunction
// is the whole edition seam: a Community build answers false to the second
// term (deployment_registry.community.go), so an operator there who sets
// ORBEAT_DEPLOYMENT_REGISTRY gets a registry that stays off, a
// /v1/sync/config that says so, and no route to 404 against.
//
// It RETURNS the effective value, unlike every other Set* method here, and
// that is deliberate rather than a stray inconsistency: cmd/api logs one line
// at startup naming whether per-developer collection is running, and spec
// sec 9.4 makes that log line the record of the decision, since enabling the
// registry is a config change with no API route to audit. A caller logging
// its own argument instead would print "enabled" in a build that ignores it.
//
// Call it BEFORE Handler(), like every other setter: Handler() reads the field
// once to decide whether the route exists, while handleSyncConfig reads it per
// request, so a setter call after Handler() would leave the two disagreeing.
// cmd/api runs every Set* before Handler() already.
func (s *Server) SetDeploymentRegistry(on bool) bool {
	s.deploymentRegistry = on && deploymentRegistrySupported()
	return s.deploymentRegistry
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

// maxBytesMiddleware wraps POST/PUT/PATCH bodies in http.MaxBytesReader, closing the
// unbounded-body vector at the transport boundary — the actual enforcement is
// on every Read from r.Body, so it protects both a huge single payload and a
// slow-dripped one. GET/DELETE carry no body in this API, so they're left
// untouched. decodeJSONOrFail (admin_servers.go) maps the resulting
// *http.MaxBytesError to 413.
func maxBytesMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// rejectVirtualKeys is the tools-door-only gate (docs/specs/2026-08-25-
// orbeat-virtual-keys-design.md sec 9): a virtual key's token is a genuine,
// validly-signed Keycloak JWT, so it passes RequireAuth like any other
// bearer token -- but it must never reach anything under this API, only the
// gateway's /mcp. This is the ONE predicate for that rule; every route that
// authenticates goes through it (see api.go's `authed`/`admin` closures and
// GET /v1/me below), rather than each route carrying its own check.
//
// WHY HERE, NOT internal/auth/middleware.go, EVEN THOUGH auth.Validator.
// RequireAuth is the literal shared chokepoint every authenticated route in
// this package passes through. Two reasons, either alone sufficient:
//
//  1. Determining "is this ClientID a virtual key" needs a tenant-scoped
//     store lookup (isVirtualKeyClient below), and internal/auth.Validator
//     has no store access at all -- it only verifies JWTs. Teaching it to
//     query Postgres would turn a pure token-validation package into one
//     with a database dependency, for a rule that only one of its two
//     callers needs.
//  2. internal/auth is genuinely shared: cmd/api AND cmd/gateway both import
//     it. But RequireAuth itself, empirically, is not -- grep the tree and
//     the only caller of (*Validator).RequireAuth anywhere is this package;
//     internal/gateway authenticates through its own verifier
//     (internal/gateway/verifier.go), which calls Validator.Validate
//     directly and never touches RequireAuth. Putting the gate inside
//     RequireAuth would rely on that fact staying true forever with nothing
//     enforcing it -- the day the gateway's transport ever routed through
//     RequireAuth instead of its own verifier, virtual keys would silently
//     stop working at /mcp, the one place they must succeed. Keeping the
//     gate here instead means there is no code path AT ALL from cmd/gateway
//     into it: internal/api is not imported by cmd/gateway or
//     internal/gateway, so the two binaries' behavior cannot drift by a
//     shared-package edit no matter what either side's auth flow does next.
//
// isVirtualKeyClient is the .ee.go/.community.go paired extension point
// (virtual_key_gate.ee.go), the same shape
// internal/gateway/virtualkey.ee.go's resolveVirtualKey/keyRevoked already
// established: identical method name and signature declared once in the
// Enterprise file and once in the Community stub, so THIS file (shared,
// non-.ee.go) can call s.isVirtualKeyClient unconditionally without ever
// naming store.VirtualKey itself. See that file's doc comment for exactly
// why a plain method pair is safe here where an interface was not
// (dcrRegisterFunc's doc comment above).
//
// Placement in the chain: AFTER RequireAuth (needs the Principal) and
// rateLimited (a flood of denied virtual-key probes is throttled like any
// other caller, and rateLimited itself does no DB write), and BEFORE
// resolver.Middleware -- mirroring RequireRole's placement in the admin
// closure below (audit B4): a request this gate is about to deny must never
// pay for Resolve's checkSeatCap/UpsertUser/role-reconcile writes. It runs
// AFTER RequireRole for admin routes too, deliberately: a virtual key's
// token is not expected to carry the orbeat-admin realm role, but this gate
// does not rely on that -- it denies on ClientID matching a virtual_key row
// alone, independent of whatever roles happen to be on the token.
//
// Fails closed: a store error while resolving isVirtualKeyClient denies the
// request (500), the same choice resolver.Middleware already makes for its
// own resolve errors.
func (s *Server) rejectVirtualKeys(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		isKey, err := s.isVirtualKeyClient(r.Context(), p.ClientID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if isKey {
			writeError(w, http.StatusForbidden, "virtual keys may only call tools through the gateway")
			return
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
	// resolver below. rejectVirtualKeys sits between the two for the identical
	// reason (see its own doc comment above): a virtual key must never reach
	// resolver.Middleware's DB writes either.
	authed := func(h http.HandlerFunc) http.Handler {
		return s.validator.RequireAuth(s.rateLimited(s.rejectVirtualKeys(s.resolver.Middleware(recordIdentity(h)))))
	}
	// Admin: authenticated + must carry the orbeat-admin realm role + resolved.
	// RequireRole runs BEFORE resolver.Middleware (audit B4): it decides purely
	// from the token's realm roles (see authz.RequireRole), so a non-admin's
	// 403 no longer pays for the tenant/user upsert + role reconcile — those
	// three queries now only run for requests that pass the role gate. Handlers
	// still see a fully resolved context (via s.resolved), since
	// resolver.Middleware directly wraps the handler in this order too.
	// rateLimited sits between RequireRole and the resolver for the same
	// before-the-DB reason as the authed closure above; rejectVirtualKeys sits
	// after it for the same reason it does in authed.
	admin := func(h http.HandlerFunc) http.Handler {
		return s.validator.RequireAuth(
			authz.RequireRole("orbeat-admin")(s.rateLimited(s.rejectVirtualKeys(s.resolver.Middleware(recordIdentity(http.HandlerFunc(h)))))))
	}

	// curated is admin PLUS the artifact-manager role: the same chain in the
	// same order, widened by exactly one name. It gates the ARTIFACT surface
	// (content and who receives it) so curating what the org's agents are told
	// does not require the ability to add an MCP server or mint a robot
	// credential.
	//
	// orbeat-admin stays a superset, so nothing an admin could do before this
	// becomes unavailable, and artifactManagerRole() is "" in a Community build,
	// where RequireAnyRole ignores it and this closure is admin-only again.
	//
	// Artifact ENTITLEMENTS are in here deliberately, and it is the one call
	// worth arguing: granting an artifact to a role looks like access control,
	// which sounds admin-only. But a manager can already reach every synced
	// developer by setting an artifact's visibility to `org`, so withholding
	// entitlements would be a control that controls nothing while making the
	// role useless for the ordinary per-role case.
	curated := func(h http.HandlerFunc) http.Handler {
		return s.validator.RequireAuth(
			authz.RequireAnyRole("orbeat-admin", artifactManagerRole())(
				s.rateLimited(s.rejectVirtualKeys(s.resolver.Middleware(recordIdentity(http.HandlerFunc(h)))))))
	}

	// /v1/me sits outside both closures above (it does no DB resolve at all),
	// so it must be rate-limited AND gated against virtual keys explicitly
	// here or it ships unlimited / open to a robot credential.
	mux.Handle("GET /v1/me", s.validator.RequireAuth(s.rateLimited(s.rejectVirtualKeys(recordIdentity(http.HandlerFunc(s.handleMe))))))
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
	mux.Handle("PUT /v1/admin/roles/{id}", admin(s.handleUpdateRole))
	mux.Handle("DELETE /v1/admin/roles/{id}", admin(s.handleDeleteRole))

	mux.Handle("DELETE /v1/admin/users/{id}", admin(s.handleDeleteUser))

	mux.Handle("POST /v1/admin/entitlements", admin(s.handleCreateEntitlement))
	mux.Handle("GET /v1/admin/entitlements", admin(s.handleListEntitlements))
	mux.Handle("GET /v1/admin/entitlements/{id}", admin(s.handleGetEntitlement))
	mux.Handle("PUT /v1/admin/entitlements/{id}", admin(s.handleUpdateEntitlement))
	mux.Handle("DELETE /v1/admin/entitlements/{id}", admin(s.handleDeleteEntitlement))

	mux.Handle("GET /v1/admin/audit", admin(s.handleListAudit))

	mux.Handle("POST /v1/admin/artifacts", curated(s.handleCreateArtifact))
	mux.Handle("GET /v1/admin/artifacts", curated(s.handleListArtifacts))
	mux.Handle("GET /v1/admin/artifacts/{id}", curated(s.handleGetArtifact))
	mux.Handle("PUT /v1/admin/artifacts/{id}", curated(s.handleUpdateArtifact))
	mux.Handle("DELETE /v1/admin/artifacts/{id}", curated(s.handleDeleteArtifact))
	mux.Handle("POST /v1/admin/marketplace/publish", admin(s.handleMarketplacePublish))
	mux.Handle("GET /v1/admin/marketplace/status", admin(s.handleMarketplaceStatus))
	mux.Handle("POST /v1/admin/artifact-entitlements", curated(s.handleCreateArtifactEntitlement))
	mux.Handle("GET /v1/admin/artifact-entitlements", curated(s.handleListArtifactEntitlements))
	mux.Handle("DELETE /v1/admin/artifact-entitlements/{id}", curated(s.handleDeleteArtifactEntitlement))

	// The artifact review lifecycle (submit/approve/reject/withdraw), revision
	// history + rollback, audit export, the deployment registry's report and
	// admin read, and the minimum-revision floor are Enterprise-only. Kept out
	// of this file so a Community
	// build (open-core generation) can drop the file that defines them without
	// api.go naming a single Enterprise handler. authed is passed alongside
	// admin because the report route's caller is an ordinary developer, not an
	// admin.
	s.registerEnterpriseRoutes(mux, admin, curated, authed)

	return otelhttp.NewHandler(
		withIdentityCarrier(logging.Requests(s.logger, apiIdentity)(corsMiddleware(s.corsOrigins)(maxBytesMiddleware(mux)))),
		"http.server",
	)
}
