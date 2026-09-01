// Package config loads orbeat service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds settings shared by orbeat binaries.
type Config struct {
	HTTPAddr         string // address the HTTP server listens on, e.g. ":8080"
	DBURL            string // Postgres connection string
	OIDCIssuer       string // OIDC issuer URL, e.g. http://localhost:8088/realms/orbeat
	OIDCAudience     string // this resource server's OAuth 2.1 audience, e.g. orbeat-api
	OIDCDiscoveryURL string // optional server-reachable OIDC base URL (ORBEAT_OIDC_DISCOVERY_URL); defaults to OIDCIssuer when empty
	TenantName       string // name of the single tenant to resolve (ORBEAT_TENANT_NAME, default "default")

	GatewayResourceURL string // the gateway's public resource URL advertised in RFC 9728 metadata (ORBEAT_GATEWAY_RESOURCE_URL, default "http://localhost:8090")

	// MarketplaceGitURL is where orbeat publishes the generated marketplace.
	// A local filesystem path (dev/CI: commit-in-place) or a remote URL (prod: push).
	// Empty disables artifact publishing.
	MarketplaceGitURL string
	// MarketplaceGitCredentialRef is a SecretsProvider ref for the git push token
	// (remote targets only; e.g. "env:ORBEAT_GIT_TOKEN"). Empty for local targets.
	MarketplaceGitCredentialRef string
	// MarketplaceGitTimeout bounds a single publish run (clone/fetch/push +
	// secrets resolve). Go duration; ORBEAT_MARKETPLACE_GIT_TIMEOUT, default "120s".
	MarketplaceGitTimeout string

	// Audit retention: prune audit_event rows older than N days. "0" (default)
	// = keep forever (retention off). Parsed by AuditRetentionDaysN.
	AuditRetentionDays string
	// How often the retention job runs (Go duration). Default 24h.
	AuditRetentionInterval string
	// Max rows a single audit export streams. "0" = unlimited. Default 100000.
	AuditExportMaxRows string

	// ArtifactRevisionKeep is how many artifact_revision rows insertRevision
	// keeps per artifact, pruning the rest in the same transaction that
	// appends a new one. "0" (default) = unlimited — no pruning, so existing
	// deployments are unchanged. Parsed by ArtifactRevisionKeepN.
	ArtifactRevisionKeep string

	// DeploymentRegistry switches on POST /v1/sync/deployments, the artifact
	// deployment registry (docs/specs/2026-08-22-orbeat-artifact-deployment-
	// registry-design.md sec 8.6). Empty (default) = OFF, and the default is
	// the point: enabling it starts recording, per named developer and per
	// machine, which artifacts that machine holds and when it last checked
	// in. Parsed by DeploymentRegistryEnabled. Enterprise only, so setting it
	// in a Community build changes nothing (internal/api's
	// SetDeploymentRegistry, which is where the two terms meet).
	DeploymentRegistry string

	// DeploymentRetentionDays is how many days an artifact_deployment row
	// survives without a fresh report before the retention loop deletes it.
	// Empty (unset) means the 90-day default, which INVERTS
	// AuditRetentionDays' off-by-default (spec sec 7.2): an audit row is a
	// compliance record whose deletion is a loss, a deployment row is a claim
	// about a person's machine that decays, and a silent install is not
	// evidence of a deployment. "0" = keep forever, accepted but warned at
	// startup by cmd/api. Parsed by DeploymentRetentionDaysN. Read only by
	// cmd/api, and only in an Enterprise build.
	//
	// Loaded with os.Getenv rather than getenv(..., "90") on purpose. The
	// sibling AuditExportMaxRows writes its default in BOTH places, and a
	// number in two places is one nobody can mutate a test against: with
	// Load() supplying "90", the accessor's empty branch is unreachable
	// through Load and a mutant that breaks it stays green. The accessor
	// below is the single owner of the number.
	DeploymentRetentionDays string

	// RateLimitRPS is the configured per-key steady rate (requests/sec).
	// Empty means unset — the per-binary default passed to RateLimitRPSN
	// wins. An explicit "0" disables limiting, matching the
	// ORBEAT_AUDIT_EXPORT_MAX_ROWS sentinel style. Parsed by RateLimitRPSN.
	RateLimitRPS string
	// RateLimitBurst is the configured per-key bucket capacity. Empty means
	// unset. Parsed by RateLimitBurstN, which clamps to >= 1.
	RateLimitBurst string
	// RateLimitInitRPS is the gateway-only budget for session-creating
	// `initialize` calls (spec §4.3). Empty means unset. Parsed by
	// RateLimitInitRPSN.
	RateLimitInitRPS string
	// GatewayMaxInflight is the gateway-only per-principal cap on concurrent
	// `tools/call` — a different axis from RateLimit*, which bounds calls
	// per second rather than how many are in flight at once (fable-audit §7
	// #14). Empty means unset. Parsed by GatewayMaxInflightN.
	GatewayMaxInflight string

	// Intercept switches on runtime call interception (docs/specs/2026-08-25-
	// orbeat-runtime-interception-design.md): the gateway scans tool call
	// arguments and results through govern.Scanner, denying an argument or
	// replacing a result on a blocking finding. ORBEAT_INTERCEPT, "" =
	// disabled (default) -- mirrors how OTelEndpoint and ScanLLMEndpoint
	// express optionality above. Unset means the hook is never INSTALLED at
	// all -- no scan, no latency, no behaviour change -- not merely
	// installed with nothing to find; cmd/gateway's wiring (interceptorFor,
	// main.go) is what turns that guarantee from a doc claim into code. Read
	// only by cmd/gateway: internal/gateway/intercept.go is deliberately
	// shared rather than Enterprise-gated (its own doc comment explains why),
	// so this knob has the same behaviour in both editions.
	Intercept string

	// CORSOrigins is the exact-match allow-list for browser cross-origin calls
	// (ORBEAT_CORS_ORIGINS, comma-separated). Unset → nil → fail closed.
	CORSOrigins []string

	LogFormat string // ORBEAT_LOG_FORMAT, "json" (default) | "text"
	LogLevel  string // ORBEAT_LOG_LEVEL, debug|info|warn|error (default "info")

	OTelEndpoint    string // ORBEAT_OTEL_ENDPOINT, "" = disabled (default)
	OTelServiceName string // ORBEAT_OTEL_SERVICE_NAME, default set per-binary in main
	OTelSampleRatio string // ORBEAT_OTEL_SAMPLE_RATIO, "1.0" default (parsed in telemetry)
	// OTelInsecure controls whether the OTLP/gRPC exporters skip TLS.
	// ORBEAT_OTEL_INSECURE, default "true" (matches the pre-existing
	// hard-coded WithInsecure() behavior, so unset deployments keep working
	// unchanged); "false" verifies the collector's certificate against the
	// system root CA pool. Parsed in telemetry.Config.ParseInsecure.
	OTelInsecure string

	// Optional LLM content scanner (semantic governance judge at submit).
	// ScanLLMEndpoint empty (default) disables it — rule scanner only.
	ScanLLMEndpoint string // ORBEAT_SCAN_LLM_ENDPOINT
	ScanLLMProvider string // ORBEAT_SCAN_LLM_PROVIDER, "openai" (default) | "anthropic"
	ScanLLMModel    string // ORBEAT_SCAN_LLM_MODEL
	ScanLLMKeyRef   string // ORBEAT_SCAN_LLM_KEY_REF, a secrets ref (env:/vault:/awssm:)
	ScanLLMTimeout  string // ORBEAT_SCAN_LLM_TIMEOUT, Go duration (default "15s")

	// ContactEmail is the operator-configured Community-edition 402
	// cap-response contact address (ORBEAT_CONTACT_EMAIL). Trimmed, but
	// otherwise passed through as-is, including empty: api.New and
	// authz.NewResolver already default their own contactEmail field to
	// authz.DefaultContactEmail, and both SetContactEmail methods ignore an
	// empty string, so this package holds no default of its own and does
	// not import internal/authz for one. cmd/gateway also calls Load() and
	// never reads this field.
	ContactEmail string

	// DCRClientID is the Keycloak service-account client id orbeat-api
	// authenticates as when registering/deleting virtual-key robot
	// credentials via Dynamic Client Registration (docs/specs/2026-08-25-
	// orbeat-virtual-keys-design.md sec 4; deploy/keycloak/orbeat-realm.json's
	// "orbeat-dcr" client, whose service account holds only realm-management's
	// create-client and view-realm roles; view-realm is not optional, see
	// internal/keycloak/dcr.ee.go). ORBEAT_DCR_CLIENT_ID. Empty (default) disables
	// virtual-key registration entirely -- no deployment that doesn't grant
	// orbeat this Keycloak privilege needs to set either of these two
	// fields, and POST /v1/admin/virtual-keys keeps refusing through
	// admin_virtual_keys.ee.go's existing nil-registrar branch. Read only by
	// cmd/api (Enterprise only; cmd/gateway calls Load() too and never reads
	// this field, matching ContactEmail's own note above).
	DCRClientID string
	// DCRClientSecretRef is a secrets ref (env:/vault:/awssm:) for
	// DCRClientID's own client secret -- the credential orbeat uses to
	// authenticate ITSELF to Keycloak's token endpoint, resolved via
	// internal/secrets the same way ScanLLMKeyRef is. This is never the
	// robot's credential (internal/keycloak.DCRClient's own doc comment on
	// ServiceAccountClientSecret draws that line). ORBEAT_DCR_CLIENT_SECRET_REF.
	DCRClientSecretRef string

	// UsageFlushInterval is how often cmd/gateway's usage counter flushes to
	// Postgres and the quota cache refreshes (docs/specs/2026-08-25-orbeat-
	// usage-metering-design.md section 3) -- ORBEAT_USAGE_FLUSH_INTERVAL, a
	// Go duration, default "60s". This is the "up to one interval" in the
	// design's stated overshoot ("a quota can be overshot by up to one flush
	// interval's worth of calls"): shortening it tightens how quickly a
	// newly-created quota takes effect and how quickly a stopped gateway's
	// in-memory counts would have been lost without the shutdown flush;
	// lengthening it trades that responsiveness for fewer round trips against
	// Postgres. Unlike ORBEAT_INTERCEPT's off-by-default sentinel, there is
	// no off value here: counting itself is unconditional (see
	// UsageFlushIntervalDuration's doc comment for why), so this only tunes
	// its cadence. Read only by cmd/gateway.
	UsageFlushInterval string

	// UsageRetentionDays is how many days a usage_daily row survives before
	// cmd/api's retention loop deletes it. Empty (unset) means the 90-day
	// default; ORBEAT_USAGE_RETENTION_DAYS=0 keeps rows forever. Mirrors
	// DeploymentRetentionDays' shape and its on-by-default direction exactly
	// (spec sec 4: "usage rows accumulate forever otherwise") -- a
	// usage_daily row is a per-day, per-tool count, not a compliance record
	// the way an audit_event row is, so ORBEAT_AUDIT_RETENTION_DAYS' off-by-
	// default direction is the wrong precedent here; the deployment
	// registry's is the right one. Read only by cmd/api, and only in an
	// Enterprise build (usage_daily itself is Enterprise-only).
	UsageRetentionDays string
}

// Load reads configuration from the environment, applying defaults.
// It returns an error if a required value is missing.
// OIDC fields are loaded but not required here; call RequireOIDC on binaries
// that validate tokens (e.g. orbeat-api).
func Load() (Config, error) {
	c := Config{
		HTTPAddr:         getenv("ORBEAT_HTTP_ADDR", ":8080"),
		DBURL:            os.Getenv("ORBEAT_DB_URL"),
		OIDCIssuer:       os.Getenv("ORBEAT_OIDC_ISSUER"),
		OIDCAudience:     os.Getenv("ORBEAT_OIDC_AUDIENCE"),
		OIDCDiscoveryURL: os.Getenv("ORBEAT_OIDC_DISCOVERY_URL"),
		TenantName:       getenv("ORBEAT_TENANT_NAME", "default"),

		GatewayResourceURL:          getenv("ORBEAT_GATEWAY_RESOURCE_URL", "http://localhost:8090"),
		MarketplaceGitURL:           os.Getenv("ORBEAT_MARKETPLACE_GIT_URL"),
		MarketplaceGitCredentialRef: os.Getenv("ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF"),
		MarketplaceGitTimeout:       getenv("ORBEAT_MARKETPLACE_GIT_TIMEOUT", "120s"),
		CORSOrigins:                 parseCORSOrigins(os.Getenv("ORBEAT_CORS_ORIGINS")),

		AuditRetentionDays:     getenv("ORBEAT_AUDIT_RETENTION_DAYS", "0"),
		AuditRetentionInterval: getenv("ORBEAT_AUDIT_RETENTION_INTERVAL", "24h"),
		AuditExportMaxRows:     getenv("ORBEAT_AUDIT_EXPORT_MAX_ROWS", "100000"),

		ArtifactRevisionKeep: getenv("ORBEAT_ARTIFACT_REVISION_KEEP", "0"),

		DeploymentRegistry:      os.Getenv("ORBEAT_DEPLOYMENT_REGISTRY"),
		DeploymentRetentionDays: os.Getenv("ORBEAT_DEPLOYMENT_RETENTION_DAYS"),

		RateLimitRPS:       os.Getenv("ORBEAT_RATELIMIT_RPS"),
		RateLimitBurst:     os.Getenv("ORBEAT_RATELIMIT_BURST"),
		RateLimitInitRPS:   os.Getenv("ORBEAT_RATELIMIT_INIT_RPS"),
		GatewayMaxInflight: os.Getenv("ORBEAT_GATEWAY_MAX_INFLIGHT"),
		Intercept:          os.Getenv("ORBEAT_INTERCEPT"),

		LogFormat: getenv("ORBEAT_LOG_FORMAT", "json"),
		LogLevel:  getenv("ORBEAT_LOG_LEVEL", "info"),

		OTelEndpoint:    os.Getenv("ORBEAT_OTEL_ENDPOINT"),
		OTelServiceName: os.Getenv("ORBEAT_OTEL_SERVICE_NAME"),
		OTelSampleRatio: getenv("ORBEAT_OTEL_SAMPLE_RATIO", "1.0"),
		OTelInsecure:    getenv("ORBEAT_OTEL_INSECURE", "true"),

		ScanLLMEndpoint: os.Getenv("ORBEAT_SCAN_LLM_ENDPOINT"),
		ScanLLMProvider: getenv("ORBEAT_SCAN_LLM_PROVIDER", "openai"),
		ScanLLMModel:    os.Getenv("ORBEAT_SCAN_LLM_MODEL"),
		ScanLLMKeyRef:   os.Getenv("ORBEAT_SCAN_LLM_KEY_REF"),
		ScanLLMTimeout:  getenv("ORBEAT_SCAN_LLM_TIMEOUT", "15s"),

		ContactEmail: strings.TrimSpace(os.Getenv("ORBEAT_CONTACT_EMAIL")),

		DCRClientID:        os.Getenv("ORBEAT_DCR_CLIENT_ID"),
		DCRClientSecretRef: os.Getenv("ORBEAT_DCR_CLIENT_SECRET_REF"),

		UsageFlushInterval: getenv("ORBEAT_USAGE_FLUSH_INTERVAL", "60s"),
		UsageRetentionDays: os.Getenv("ORBEAT_USAGE_RETENTION_DAYS"),
	}
	if c.DBURL == "" {
		return Config{}, errors.New("ORBEAT_DB_URL is required")
	}
	if err := checkMarketplaceGitURL(c.MarketplaceGitURL); err != nil {
		return Config{}, err
	}
	return c, nil
}

// checkMarketplaceGitURL rejects a MarketplaceGitURL that embeds credentials
// as URL userinfo (https://user:token@host/...). Persisting/publishing from
// such a URL leaks the token into publish_state, audit metadata, and any
// error message that echoes the URL (audit G12) — the credential belongs in
// ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF (a SecretsProvider ref), never the
// URL. Local filesystem paths and SCP-like "git@host:path" syntax (no
// "://", conventional SSH login, key-based auth) are left alone.
//
// The credential test is a STRING scan for "@" after the scheme, run before any
// parse, and it replaces the u.User != nil test this guard used to make. That
// is not a stylistic swap. url.Parse FAILS on the exact URLs this guard exists
// to reject: a token containing "/" makes the authority unparseable, so
// url.Parse("https://x-access-token:b64/token+val@github.com/o/r") returns
// `invalid port ":b64" after host`, and the old `if err != nil ... return nil`
// treated that as acceptable. A parse error meant ACCEPTED. It is the same
// fail-open shape audit A6 fixed one layer down in internal/publish, and the
// scan here is that fix's rule applied at the same width: within a
// "scheme://..." string, everything up to the last "@" is userinfo.
//
// Cost of the over-approximation, stated because it is real: a URL whose PATH
// contains "@" and carries no credential is refused, so
// "https://github.com/org/repo@v1.git" does not start. internal/publish already
// accepted the same cost (it over-redacts that URL), and the two layers now
// agree on one rule rather than each guessing. Refusing at startup with a named
// variable is also the recoverable end of the trade: the operator sees the
// message immediately, where a leaked push token is not recoverable at all.
// Note that "ssh://git@host/repo.git" was already refused by the u.User test
// this replaces; only the path-"@" case is newly refused.
//
// Neither message echoes raw, and the parse failure is NOT wrapped: url.Error's
// own text quotes the whole URL, which on the motivating input is the token.
func checkMarketplaceGitURL(raw string) error {
	if raw == "" || !strings.Contains(raw, "://") {
		return nil
	}
	if _, afterScheme, _ := strings.Cut(raw, "://"); strings.Contains(afterScheme, "@") {
		return fmt.Errorf("ORBEAT_MARKETPLACE_GIT_URL must not embed credentials (user:token@); set ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF instead")
	}
	if _, err := url.Parse(raw); err != nil {
		return errors.New("ORBEAT_MARKETPLACE_GIT_URL is not a parseable URL (the value is not echoed: it may carry a credential)")
	}
	return nil
}

// MarketplaceGitTimeoutDuration parses the configured publish timeout. It falls
// back to 120s on any malformed or non-positive value, so a bad config can never
// produce an unbounded (0) publish run. ok is false when the fallback was used
// (the caller may warn).
func (c Config) MarketplaceGitTimeoutDuration() (d time.Duration, ok bool) {
	d, err := time.ParseDuration(c.MarketplaceGitTimeout)
	if err != nil || d <= 0 {
		return 120 * time.Second, false
	}
	return d, true
}

// AuditRetentionDaysN is the retention window in days; 0 = off (keep forever).
func (c Config) AuditRetentionDaysN() int {
	n, err := strconv.Atoi(strings.TrimSpace(c.AuditRetentionDays))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// AuditRetentionIntervalDuration is how often the prune job runs (default 24h).
func (c Config) AuditRetentionIntervalDuration() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(c.AuditRetentionInterval))
	if err != nil || d <= 0 {
		return 24 * time.Hour
	}
	return d
}

// AuditExportMaxRowsN is the export cap; 0 = unlimited. Default 100000.
func (c Config) AuditExportMaxRowsN() int {
	s := strings.TrimSpace(c.AuditExportMaxRows)
	if s == "" {
		return 100000
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 100000
	}
	return n
}

// ArtifactRevisionKeepN is how many revisions per artifact insertRevision
// keeps; 0 = unlimited (no pruning). Mirrors AuditRetentionDaysN exactly — no
// numeric accessor in this file returns an error, and this one must not be
// the first: Load() is also called by cmd/gateway, which never reads this
// knob, so a strict-fail here would stop the gateway starting over a value it
// does not use.
func (c Config) ArtifactRevisionKeepN() int {
	n, err := strconv.Atoi(strings.TrimSpace(c.ArtifactRevisionKeep))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// DeploymentRegistryEnabled reports whether the operator asked for the
// artifact deployment registry. Only an explicit, unambiguous affirmative
// counts: "1", "t", "true", "T", "TRUE" and the rest of strconv.ParseBool's
// accepted set, case-insensitively. Anything else, including unset, empty,
// "yes", "on" and a typo, is OFF.
//
// Unparseable means off rather than an error, mirroring every other accessor
// in this file: Load() also runs in cmd/gateway, which never reads this knob,
// so a strict failure would stop the gateway starting over a value it does not
// use. The direction the leniency falls matters here more than elsewhere,
// because this knob turns on per-developer collection: a value nobody can read
// as a clear yes must never be read as one.
func (c Config) DeploymentRegistryEnabled() bool {
	on, err := strconv.ParseBool(strings.TrimSpace(c.DeploymentRegistry))
	return err == nil && on
}

// DeploymentRetentionDaysN is the deployment-registry retention window in
// days; 0 = off (keep forever). It defaults to 90, which is the one retention
// default in this file that is ON, and the asymmetry with AuditRetentionDaysN
// is deliberate rather than an oversight (spec sec 7.2): an audit row is a
// compliance record and deleting it is a loss, a deployment row is a claim
// about a named person's machine that stops being true the moment that
// machine goes quiet.
//
// THE DIRECTION THE LENIENCY FALLS IS THE OPPOSITE OF AuditRetentionDaysN's,
// and that follows from the same principle rather than contradicting it.
// There, an unreadable value collapses to 0 so a typo can never silently
// start deleting compliance records. Here, 0 is the dangerous answer, so an
// unset, blank, negative or unparseable value collapses to the 90-day default
// and only an explicit, readable "0" keeps rows forever. That mirrors
// AuditExportMaxRowsN's shape, which is the existing precedent in this file
// for a knob whose default is a real number rather than a sentinel.
//
// Unparseable means "the default" rather than an error for the reason
// ArtifactRevisionKeepN states: Load() also runs in cmd/gateway, which never
// reads this knob, so a strict failure would stop the gateway starting over a
// value it does not use.
func (c Config) DeploymentRetentionDaysN() int {
	s := strings.TrimSpace(c.DeploymentRetentionDays)
	if s == "" {
		return 90
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 90
	}
	return n
}

// RateLimitRPSN returns the configured steady rate, or def when unset or
// unparseable. A bad value can never produce 0 (= limiting disabled): only an
// explicit "0" does that, matching ORBEAT_AUDIT_RETENTION_DAYS' sentinel style.
func (c Config) RateLimitRPSN(def float64) float64 {
	s := strings.TrimSpace(c.RateLimitRPS)
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return def
	}
	return f
}

// RateLimitBurstN returns the configured burst, or def when unset or
// unparseable. Clamped to >= 1: x/time/rate's AllowN/ReserveN/WaitN reject
// any request when n > burst, so a literal burst of 0 would deny every
// authenticated request.
func (c Config) RateLimitBurstN(def int) int {
	s := strings.TrimSpace(c.RateLimitBurst)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	if n < 1 {
		return 1
	}
	return n
}

// RateLimitInitRPSN returns the configured session-init rate (gateway only,
// spec §4.3), or def when unset or unparseable. Same fail-safe rules as
// RateLimitRPSN: a bad value never disables limiting, only an explicit "0" does.
func (c Config) RateLimitInitRPSN(def float64) float64 {
	s := strings.TrimSpace(c.RateLimitInitRPS)
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return def
	}
	return f
}

// GatewayMaxInflightN returns the configured per-principal cap on concurrent
// `tools/call`, or def when unset or unparseable. A bad value can never
// disable the cap: only an explicit "0" does that — matching
// ratelimit.ConcurrencyLimiter's own sentinel ("max <= 0 disables the cap
// entirely"), the same style RateLimitRPSN uses for its rps <= 0 sentinel.
// Deliberately NOT RateLimitBurstN's clamp-to-1: there x/time/rate rejects
// any call once burst < n, so a literal 0 would silently deny every request —
// no such failure mode exists here, so 0 stays the honest "unlimited" value.
func (c Config) GatewayMaxInflightN(def int) int {
	s := strings.TrimSpace(c.GatewayMaxInflight)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// UsageFlushIntervalDuration is how often cmd/gateway flushes usage counts
// and refreshes the quota cache (default 60s; an unparseable or non-positive
// value falls back to it, mirroring AuditRetentionIntervalDuration's own
// fallback shape exactly).
//
// THERE IS NO OFF SWITCH FOR COUNTING ITSELF, and that is capo's explicit
// call (docs/plans/orbeat-usage-metering-2026-08-25.md Task 5), not an
// oversight: counting is cheap (an in-memory map increment, off the
// tool-call hot path by construction -- usage.ee.go's UsageCounter.Count
// never touches the database) and it IS the subsystem's entire value --
// quota enforcement reads nothing but what counting wrote, so an off-by-
// default counter would leave every role_quota row an admin ever sets
// permanently inert, the exact "nine tasks of tested code connected to
// nothing" failure mode this task's own wiring gate exists to catch, just
// moved one layer up from "not installed" to "installed but never counts
// anything". Quota ENFORCEMENT needs no separate toggle either, for the
// opposite reason: it is already off in every practical sense until an
// admin creates a role_quota row (QuotaEnforcer.Check's documented "no
// cache entry is unlimited" contract, quota.ee.go) -- gating it a second
// time at the config layer would just be two off-switches for one behavior.
func (c Config) UsageFlushIntervalDuration() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(c.UsageFlushInterval))
	if err != nil || d <= 0 {
		return 60 * time.Second
	}
	return d
}

// UsageRetentionDaysN is the usage_daily retention window in days; 0 = off
// (keep forever). Defaults to 90 -- ON, mirroring DeploymentRetentionDaysN's
// shape exactly, including which direction leniency falls (empty, negative
// or unparseable all collapse to the 90-day default; only an explicit,
// readable "0" keeps rows forever) and why: capo's stated call is that usage
// rows accumulate forever otherwise, and a usage_daily row is closer in kind
// to a deployment row (a claim that decays -- here, "how much a role called
// last spring" is of steadily shrinking operational relevance once that
// month's quota window has long closed) than to an audit_event row (a
// compliance record whose deletion is a loss). Unparseable means "the
// default" rather than an error for the reason DeploymentRetentionDaysN and
// ArtifactRevisionKeepN both give: Load() also runs in cmd/gateway, which
// never reads this knob, so a strict failure would stop the gateway starting
// over a value it does not use.
func (c Config) UsageRetentionDaysN() int {
	s := strings.TrimSpace(c.UsageRetentionDays)
	if s == "" {
		return 90
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 90
	}
	return n
}

// RequireOIDC returns an error if the OIDC settings needed by a resource
// server are not configured. Call this from binaries that validate tokens.
func (c Config) RequireOIDC() error {
	if c.OIDCIssuer == "" {
		return errors.New("ORBEAT_OIDC_ISSUER is required")
	}
	if c.OIDCAudience == "" {
		return errors.New("ORBEAT_OIDC_AUDIENCE is required")
	}
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseCORSOrigins splits a comma-separated origin list, trims spaces, and
// drops empty entries. An unset or blank value returns nil (fail closed).
func parseCORSOrigins(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
