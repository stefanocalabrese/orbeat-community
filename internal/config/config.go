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

		RateLimitRPS:       os.Getenv("ORBEAT_RATELIMIT_RPS"),
		RateLimitBurst:     os.Getenv("ORBEAT_RATELIMIT_BURST"),
		RateLimitInitRPS:   os.Getenv("ORBEAT_RATELIMIT_INIT_RPS"),
		GatewayMaxInflight: os.Getenv("ORBEAT_GATEWAY_MAX_INFLIGHT"),

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
func checkMarketplaceGitURL(raw string) error {
	if raw == "" || !strings.Contains(raw, "://") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return nil
	}
	return fmt.Errorf("ORBEAT_MARKETPLACE_GIT_URL must not embed credentials (user:token@); set ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF instead")
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
