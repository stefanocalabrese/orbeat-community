package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("ORBEAT_HTTP_ADDR", "")
	t.Setenv("ORBEAT_DB_URL", "postgres://example")
	t.Setenv("ORBEAT_OIDC_ISSUER", "http://kc/realms/orbeat")
	t.Setenv("ORBEAT_OIDC_AUDIENCE", "orbeat-api")

	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.HTTPAddr != ":8080" {
		t.Fatalf("HTTPAddr = %q, want \":8080\"", c.HTTPAddr)
	}
	if c.DBURL != "postgres://example" {
		t.Fatalf("DBURL = %q, want \"postgres://example\"", c.DBURL)
	}
}

func TestLoadRequiresDBURL(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "")
	t.Setenv("ORBEAT_OIDC_ISSUER", "http://kc/realms/orbeat")
	t.Setenv("ORBEAT_OIDC_AUDIENCE", "orbeat-api")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when ORBEAT_DB_URL is unset, got nil")
	}
}

func TestLoadOIDC(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_OIDC_ISSUER", "http://kc/realms/orbeat")
	t.Setenv("ORBEAT_OIDC_AUDIENCE", "orbeat-api")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OIDCIssuer != "http://kc/realms/orbeat" || c.OIDCAudience != "orbeat-api" {
		t.Fatalf("oidc = %q / %q", c.OIDCIssuer, c.OIDCAudience)
	}
}

func TestLoadTenantNameDefault(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_TENANT_NAME", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.TenantName != "default" {
		t.Fatalf("TenantName = %q, want default", c.TenantName)
	}
}

func TestLoadGatewayResourceURLDefault(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_GATEWAY_RESOURCE_URL", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GatewayResourceURL != "http://localhost:8090" {
		t.Fatalf("GatewayResourceURL = %q, want http://localhost:8090", c.GatewayResourceURL)
	}
}

func TestLoadCORSOrigins(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "x")
	t.Setenv("ORBEAT_CORS_ORIGINS", "http://a:1, http://b:2")
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.CORSOrigins) != 2 || c.CORSOrigins[0] != "http://a:1" || c.CORSOrigins[1] != "http://b:2" {
		t.Fatalf("CORSOrigins = %v", c.CORSOrigins)
	}
}

func TestLoadOIDCDiscoveryURL(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_OIDC_ISSUER", "http://localhost:8088/realms/orbeat")
	t.Setenv("ORBEAT_OIDC_AUDIENCE", "orbeat-api")
	t.Setenv("ORBEAT_OIDC_DISCOVERY_URL", "http://keycloak:8080/realms/orbeat")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OIDCDiscoveryURL != "http://keycloak:8080/realms/orbeat" {
		t.Fatalf("OIDCDiscoveryURL = %q, want http://keycloak:8080/realms/orbeat", c.OIDCDiscoveryURL)
	}
}

func TestLoadOIDCDiscoveryURLDefaultsEmpty(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_OIDC_DISCOVERY_URL", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OIDCDiscoveryURL != "" {
		t.Fatalf("OIDCDiscoveryURL = %q, want empty", c.OIDCDiscoveryURL)
	}
}

func TestLoadMarketplaceGitDefaults(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", "")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.MarketplaceGitURL != "" || c.MarketplaceGitCredentialRef != "" {
		t.Fatalf("want empty defaults, got %q / %q", c.MarketplaceGitURL, c.MarketplaceGitCredentialRef)
	}
}

func TestLoadLogDefaults(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_LOG_FORMAT", "")
	t.Setenv("ORBEAT_LOG_LEVEL", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LogFormat != "json" || c.LogLevel != "info" {
		t.Fatalf("bad log defaults: %q %q", c.LogFormat, c.LogLevel)
	}
}

func TestLoadLogOverrides(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_LOG_FORMAT", "text")
	t.Setenv("ORBEAT_LOG_LEVEL", "debug")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LogFormat != "text" || c.LogLevel != "debug" {
		t.Fatalf("overrides not applied: %q %q", c.LogFormat, c.LogLevel)
	}
}

func TestLoadScanLLMDefaults(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	// ensure a clean slate
	for _, k := range []string{"ORBEAT_SCAN_LLM_ENDPOINT", "ORBEAT_SCAN_LLM_PROVIDER",
		"ORBEAT_SCAN_LLM_MODEL", "ORBEAT_SCAN_LLM_KEY_REF", "ORBEAT_SCAN_LLM_TIMEOUT"} {
		t.Setenv(k, "")
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ScanLLMEndpoint != "" {
		t.Fatalf("endpoint default = %q, want empty (disabled)", c.ScanLLMEndpoint)
	}
	if c.ScanLLMProvider != "openai" {
		t.Fatalf("provider default = %q, want openai", c.ScanLLMProvider)
	}
	if c.ScanLLMTimeout != "15s" {
		t.Fatalf("timeout default = %q, want 15s", c.ScanLLMTimeout)
	}
}

func TestLoadScanLLMOverrides(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_SCAN_LLM_ENDPOINT", "https://llm.internal")
	t.Setenv("ORBEAT_SCAN_LLM_PROVIDER", "anthropic")
	t.Setenv("ORBEAT_SCAN_LLM_MODEL", "some-model")
	t.Setenv("ORBEAT_SCAN_LLM_KEY_REF", "vault:secret/orbeat/llm#key")
	t.Setenv("ORBEAT_SCAN_LLM_TIMEOUT", "30s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ScanLLMEndpoint != "https://llm.internal" || c.ScanLLMProvider != "anthropic" ||
		c.ScanLLMModel != "some-model" || c.ScanLLMKeyRef != "vault:secret/orbeat/llm#key" ||
		c.ScanLLMTimeout != "30s" {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestLoadDCRDefaults(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_DCR_CLIENT_ID", "")
	t.Setenv("ORBEAT_DCR_CLIENT_SECRET_REF", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DCRClientID != "" || c.DCRClientSecretRef != "" {
		t.Fatalf("want empty DCR defaults (disabled), got %q / %q", c.DCRClientID, c.DCRClientSecretRef)
	}
}

func TestLoadDCROverrides(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_DCR_CLIENT_ID", "orbeat-dcr")
	t.Setenv("ORBEAT_DCR_CLIENT_SECRET_REF", "vault:secret/orbeat/dcr#secret")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DCRClientID != "orbeat-dcr" || c.DCRClientSecretRef != "vault:secret/orbeat/dcr#secret" {
		t.Fatalf("overrides not applied: %q / %q", c.DCRClientID, c.DCRClientSecretRef)
	}
}

// TestLoadInterceptDefault pins Intercept's off-by-default shape (design
// spec §4): unset ORBEAT_INTERCEPT must load as the empty string, the same
// sentinel OTelEndpoint and ScanLLMEndpoint use for "disabled" -- cmd/gateway
// reads this exact emptiness to decide whether to install the interceptor at
// all (Task 4's wiring gate covers that half; this pins the config half).
func TestLoadInterceptDefault(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_INTERCEPT", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Intercept != "" {
		t.Fatalf("want empty Intercept (disabled) by default, got %q", c.Intercept)
	}
}

func TestLoadInterceptOverride(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_INTERCEPT", "1")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Intercept != "1" {
		t.Fatalf("override not applied: got %q, want %q", c.Intercept, "1")
	}
}

func TestLoadMarketplaceGitTimeoutDefault(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_TIMEOUT", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.MarketplaceGitTimeout != "120s" {
		t.Fatalf("MarketplaceGitTimeout = %q, want 120s", c.MarketplaceGitTimeout)
	}
}

func TestMarketplaceGitTimeoutDuration(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
		ok   bool
	}{
		{"120s", 120 * time.Second, true},
		{"30s", 30 * time.Second, true},
		{"2m", 2 * time.Minute, true},
		{"", 120 * time.Second, false},    // empty
		{"abc", 120 * time.Second, false}, // garbage
		{"0s", 120 * time.Second, false},  // zero → would be unbounded
		{"-5s", 120 * time.Second, false}, // negative
	} {
		d, ok := Config{MarketplaceGitTimeout: tc.raw}.MarketplaceGitTimeoutDuration()
		if d != tc.want || ok != tc.ok {
			t.Errorf("%q: got (%v, %v), want (%v, %v)", tc.raw, d, ok, tc.want, tc.ok)
		}
	}
}

func TestLoadRejectsMarketplaceGitURLUserinfo(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", "https://user:token@example.test/repo.git")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected an error when ORBEAT_MARKETPLACE_GIT_URL embeds userinfo, got nil")
	}
	if !strings.Contains(err.Error(), "ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF") {
		t.Fatalf("error should point at ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF, got: %v", err)
	}
}

func TestLoadRejectsMarketplaceGitURLUsernameOnly(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", "https://token@example.test/repo.git")
	_, err := Load()
	if err == nil {
		t.Fatal("expected an error when ORBEAT_MARKETPLACE_GIT_URL embeds a bare username, got nil")
	}
}

func TestLoadAllowsMarketplaceGitURLWithoutUserinfo(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", "https://example.test/repo.git")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF", "env:ORBEAT_GIT_TOKEN")
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.MarketplaceGitURL != "https://example.test/repo.git" {
		t.Fatalf("MarketplaceGitURL = %q", c.MarketplaceGitURL)
	}
}

func TestLoadAllowsMarketplaceGitURLLocalPath(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", "/var/lib/orbeat/marketplace")
	if _, err := Load(); err != nil {
		t.Fatalf("unexpected error for a local path: %v", err)
	}
}

func TestLoadAllowsMarketplaceGitURLScpSyntax(t *testing.T) {
	// git@host:path SCP-like syntax has no "://" and its "user@" is the
	// conventional SSH login (key-based auth, not an embedded secret) — it
	// must NOT be rejected by the userinfo guard.
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", "git@github.com:org/repo.git")
	if _, err := Load(); err != nil {
		t.Fatalf("unexpected error for SCP-like syntax: %v", err)
	}
}

func TestAuditRetentionAndExportConfig(t *testing.T) {
	// Load() requires these three; set them so it succeeds.
	t.Setenv("ORBEAT_DB_URL", "postgres://example")
	t.Setenv("ORBEAT_OIDC_ISSUER", "http://kc/realms/orbeat")
	t.Setenv("ORBEAT_OIDC_AUDIENCE", "orbeat-api")

	t.Setenv("ORBEAT_AUDIT_RETENTION_DAYS", "30")
	t.Setenv("ORBEAT_AUDIT_RETENTION_INTERVAL", "6h")
	t.Setenv("ORBEAT_AUDIT_EXPORT_MAX_ROWS", "0")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.AuditRetentionDaysN(); got != 30 {
		t.Errorf("AuditRetentionDaysN = %d, want 30", got)
	}
	if got := c.AuditRetentionIntervalDuration(); got != 6*time.Hour {
		t.Errorf("interval = %v, want 6h", got)
	}
	if got := c.AuditExportMaxRowsN(); got != 0 {
		t.Errorf("export max = %d, want 0 (unlimited)", got)
	}

	// Defaults when unset: 0 days (off), 24h, 100000.
	t.Setenv("ORBEAT_AUDIT_RETENTION_DAYS", "")
	t.Setenv("ORBEAT_AUDIT_RETENTION_INTERVAL", "")
	t.Setenv("ORBEAT_AUDIT_EXPORT_MAX_ROWS", "")
	d, err := Load()
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if d.AuditRetentionDaysN() != 0 || d.AuditRetentionIntervalDuration() != 24*time.Hour || d.AuditExportMaxRowsN() != 100000 {
		t.Errorf("defaults wrong: days=%d interval=%v max=%d",
			d.AuditRetentionDaysN(), d.AuditRetentionIntervalDuration(), d.AuditExportMaxRowsN())
	}

	// Bad values fall back to the safe default (never panic).
	t.Setenv("ORBEAT_AUDIT_RETENTION_DAYS", "notanint")
	t.Setenv("ORBEAT_AUDIT_RETENTION_INTERVAL", "garbage")
	t.Setenv("ORBEAT_AUDIT_EXPORT_MAX_ROWS", "-5")
	b, err := Load()
	if err != nil {
		t.Fatalf("Load bad: %v", err)
	}
	if b.AuditRetentionDaysN() != 0 || b.AuditRetentionIntervalDuration() != 24*time.Hour || b.AuditExportMaxRowsN() != 100000 {
		t.Errorf("bad-value fallback wrong: days=%d interval=%v max=%d",
			b.AuditRetentionDaysN(), b.AuditRetentionIntervalDuration(), b.AuditExportMaxRowsN())
	}
}

func TestRateLimitParsing(t *testing.T) {
	// Load() requires ORBEAT_DB_URL; set it once for the whole test, it
	// persists across the t.Run subtests below (t.Setenv on the parent test
	// restores only after the parent test — including its subtests — finishes).
	t.Setenv("ORBEAT_DB_URL", "postgres://example")

	for _, tc := range []struct {
		name       string
		rps, burst string
		wantRPS    float64
		wantBurst  int
	}{
		{"defaults when unset", "", "", 50, 200},
		{"explicit", "10", "20", 10, 20},
		{"zero rps disables", "0", "20", 0, 20},
		{"garbage rps falls back to the default, never to off", "abc", "20", 50, 20},
		{"negative rps falls back", "-5", "20", 50, 20},
		{"burst 0 is clamped to 1, not a total outage", "10", "0", 10, 1},
		{"garbage burst falls back", "10", "xyz", 10, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBEAT_RATELIMIT_RPS", tc.rps)
			t.Setenv("ORBEAT_RATELIMIT_BURST", tc.burst)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.RateLimitRPSN(50); got != tc.wantRPS {
				t.Errorf("RateLimitRPSN = %v, want %v", got, tc.wantRPS)
			}
			if got := c.RateLimitBurstN(200); got != tc.wantBurst {
				t.Errorf("RateLimitBurstN = %v, want %v", got, tc.wantBurst)
			}
		})
	}
}

// TestRateLimitInitRPSParsing covers RateLimitInitRPSN with the same shape of
// cases as TestRateLimitParsing's RPS column. The plan this test implements
// specified the accessor but gave no test cases for it — an accessor with no
// test is exactly the defect class this repo keeps finding (cf. the pagination
// slice's ORDER BY bug, shipped inert since Phase 1 because nothing exercised it).
func TestRateLimitInitRPSParsing(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://example")

	for _, tc := range []struct {
		name    string
		initRPS string
		want    float64
	}{
		{"defaults when unset", "", 1},
		{"explicit", "2", 2},
		{"zero disables", "0", 0},
		{"garbage falls back to the default, never to off", "abc", 1},
		{"negative falls back", "-5", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBEAT_RATELIMIT_INIT_RPS", tc.initRPS)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.RateLimitInitRPSN(1); got != tc.want {
				t.Errorf("RateLimitInitRPSN = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGatewayMaxInflightParsing covers GatewayMaxInflightN with the same
// shape of cases as TestRateLimitParsing, except zero is a valid, meaningful
// value here (disables the cap) rather than clamped away like
// RateLimitBurstN's burst=0 footgun — see GatewayMaxInflightN's doc comment.
func TestGatewayMaxInflightParsing(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://example")

	for _, tc := range []struct {
		name   string
		maxInf string
		want   int
	}{
		{"defaults when unset", "", 8},
		{"explicit", "16", 16},
		{"zero disables the cap", "0", 0},
		{"garbage falls back to the default, never to off", "abc", 8},
		{"negative falls back", "-5", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBEAT_GATEWAY_MAX_INFLIGHT", tc.maxInf)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.GatewayMaxInflightN(8); got != tc.want {
				t.Errorf("GatewayMaxInflightN = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestArtifactRevisionKeepParsing covers ArtifactRevisionKeepN with the same
// shape of cases as TestGatewayMaxInflightParsing: unset/garbage/negative all
// fall back to 0 (unlimited — no pruning), matching AuditRetentionDaysN's
// fallback rule exactly. "1" is a valid, accepted value (spec §5) even though
// it leaves no rollback target.
func TestArtifactRevisionKeepParsing(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://example")

	for _, tc := range []struct {
		name string
		keep string
		want int
	}{
		{"unset defaults to 0 (unlimited)", "", 0},
		{"explicit zero", "0", 0},
		{"explicit positive", "5", 5},
		{"negative falls back to 0", "-3", 0},
		{"garbage falls back to 0", "abc", 0},
		{"one is accepted, not rejected", "1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBEAT_ARTIFACT_REVISION_KEEP", tc.keep)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.ArtifactRevisionKeepN(); got != tc.want {
				t.Errorf("ArtifactRevisionKeepN = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestContactEmailParsing covers ContactEmail: unset or whitespace-only
// passes through as empty, and a real value is trimmed. There is no
// package-level default to fall back to here: api.New and
// authz.NewResolver already default their own contactEmail field to
// authz.DefaultContactEmail, and both SetContactEmail methods ignore an
// empty string, so an empty Config.ContactEmail leaves those defaults
// untouched rather than needing this package to restate them (and it would
// have to import internal/authz to do so, which internal/config does not).
// Unlike the numeric accessors above, "garbage" has no meaning here: any
// non-blank string is a legitimate operator-chosen address, so there is no
// reject case to assert.
func TestContactEmailParsing(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://example")

	for _, tc := range []struct {
		name  string
		email string
		want  string
	}{
		{"unset stays empty", "", ""},
		{"whitespace-only trims to empty", "   ", ""},
		{"explicit value is used", "ops@example.com", "ops@example.com"},
		{"explicit value is trimmed", "  ops@example.com  ", "ops@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBEAT_CONTACT_EMAIL", tc.email)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.ContactEmail; got != tc.want {
				t.Errorf("ContactEmail = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRequireOIDC(t *testing.T) {
	t.Run("both set returns nil", func(t *testing.T) {
		c := Config{DBURL: "postgres://x", OIDCIssuer: "http://kc/realms/orbeat", OIDCAudience: "orbeat-api"}
		if err := c.RequireOIDC(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing issuer returns error", func(t *testing.T) {
		c := Config{DBURL: "postgres://x", OIDCAudience: "orbeat-api"}
		if err := c.RequireOIDC(); err == nil {
			t.Fatal("expected error when OIDCIssuer is empty, got nil")
		}
	})

	t.Run("missing audience returns error", func(t *testing.T) {
		c := Config{DBURL: "postgres://x", OIDCIssuer: "http://kc/realms/orbeat"}
		if err := c.RequireOIDC(); err == nil {
			t.Fatal("expected error when OIDCAudience is empty, got nil")
		}
	})

	t.Run("Load succeeds without OIDC vars", func(t *testing.T) {
		t.Setenv("ORBEAT_DB_URL", "postgres://x")
		t.Setenv("ORBEAT_OIDC_ISSUER", "")
		t.Setenv("ORBEAT_OIDC_AUDIENCE", "")
		if _, err := Load(); err != nil {
			t.Fatalf("Load must succeed without OIDC vars (gateway path): %v", err)
		}
	})
}

// TestDeploymentRegistryParsing covers the one knob in this file that turns on
// data collection about named people, so its table leans on the cases that
// must NOT count as a yes.
//
// "yes" and "on" are in the table deliberately. Both read as affirmative to a
// human writing a .env file and neither is accepted by strconv.ParseBool, so
// this pins that they land on OFF rather than silently enabling collection,
// and it pins that the accessor never grew a friendlier parser later.
func TestDeploymentRegistryParsing(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://example")

	for _, tc := range []struct {
		name string
		val  string
		want bool
	}{
		{"unset is off", "", false},
		{"explicit false", "false", false},
		{"explicit zero", "0", false},
		{"yes is NOT a yes", "yes", false},
		{"on is NOT a yes", "on", false},
		{"garbage is off", "maybe", false},
		{"true", "true", true},
		{"one", "1", true},
		{"TRUE", "TRUE", true},
		{"padded true is trimmed", "  true  ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBEAT_DEPLOYMENT_REGISTRY", tc.val)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.DeploymentRegistryEnabled(); got != tc.want {
				t.Errorf("DeploymentRegistryEnabled(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestDeploymentRetentionInvertsTheAuditDefault asserts the two retention
// defaults IN THE SAME Load, which is the only way to see the property that
// matters. Each on its own is unremarkable; the pair is the design decision
// (spec docs/specs/2026-08-22-orbeat-artifact-deployment-registry-design.md
// sec 7.2): audit retention is OFF unless the operator asks for it because an
// audit row is a compliance record whose deletion is a loss, deployment
// retention is ON at 90 days because a deployment row is a claim about a named
// person's machine that stops being true once that machine goes quiet.
//
// A deployments-only assertion cannot see the regression this exists to catch:
// the retention loop was extracted so both subjects share one runRetention,
// and a shared default would silently turn audit pruning ON for every existing
// operator, deleting compliance records nobody asked to delete.
//
// The bad-value rows are the second half, and they fall in OPPOSITE
// directions on purpose. For audit, an unreadable value collapses to 0 so a
// typo can never start deleting audit rows. For deployments, 0 is the
// dangerous answer, so an unreadable value collapses to 90 and only a readable
// "0" keeps rows forever.
func TestUsageFlushIntervalParsing(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://example")

	for _, tc := range []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset defaults to 60s", "", 60 * time.Second},
		{"explicit value honoured", "30s", 30 * time.Second},
		{"unparseable falls back to default", "garbage", 60 * time.Second},
		{"zero falls back to default (a zero-interval ticker is nonsensical)", "0s", 60 * time.Second},
		{"negative falls back to default", "-5s", 60 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBEAT_USAGE_FLUSH_INTERVAL", tc.env)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.UsageFlushIntervalDuration(); got != tc.want {
				t.Errorf("UsageFlushIntervalDuration(%q) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

func TestUsageRetentionDefaultsOnLikeDeploymentRetention(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://example")

	for _, tc := range []struct {
		name string
		env  string
		want int
	}{
		{"unset defaults to 90 (on)", "", 90},
		{"explicit zero keeps usage rows forever", "0", 0},
		{"explicit window is honoured", "30", 30},
		{"unparseable falls back to the default", "notanint", 90},
		{"negative falls back to the default", "-5", 90},
		{"padded value is trimmed", "  45  ", 45},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBEAT_USAGE_RETENTION_DAYS", tc.env)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.UsageRetentionDaysN(); got != tc.want {
				t.Errorf("UsageRetentionDaysN(%q) = %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

func TestDeploymentRetentionInvertsTheAuditDefault(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://example")

	for _, tc := range []struct {
		name          string
		deploymentEnv string
		auditEnv      string
		wantDeploy    int
		wantAudit     int
	}{
		{"both unset: deployments on at 90, audit off", "", "", 90, 0},
		{"explicit zero keeps deployment rows forever", "0", "", 0, 0},
		{"explicit window is honoured", "30", "30", 30, 30},
		{"unparseable falls opposite ways", "notanint", "notanint", 90, 0},
		{"negative falls opposite ways", "-5", "-5", 90, 0},
		{"padded value is trimmed", "  45  ", "", 45, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBEAT_DEPLOYMENT_RETENTION_DAYS", tc.deploymentEnv)
			t.Setenv("ORBEAT_AUDIT_RETENTION_DAYS", tc.auditEnv)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := c.DeploymentRetentionDaysN(); got != tc.wantDeploy {
				t.Errorf("DeploymentRetentionDaysN(%q) = %d, want %d", tc.deploymentEnv, got, tc.wantDeploy)
			}
			if got := c.AuditRetentionDaysN(); got != tc.wantAudit {
				t.Errorf("AuditRetentionDaysN(%q) = %d, want %d: the shared retention loop must not "+
					"have given audit retention the deployment default", tc.auditEnv, got, tc.wantAudit)
			}
		})
	}
}

// slashBearingCredentialURL is the URL that motivated the fix: a GitHub App
// installation token, base64 material, or a GitLab/Gitea deploy token routinely
// contains "/", and a "/" inside the userinfo makes the authority unparseable,
// so url.Parse returns `invalid port ":b64" after host`. The guard used to read
// that parse error as "nothing to object to" and return nil, which ACCEPTED the
// one input class it exists to reject.
const slashBearingCredentialURL = "https://x-access-token:b64/token+val@github.com/o/r"

func TestLoadRejectsMarketplaceGitURLWithASlashBearingCredential(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", slashBearingCredentialURL)
	t.Setenv("ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF", "")
	_, err := Load()
	if err == nil {
		t.Fatal("a credential containing '/' was accepted: url.Parse fails on it, and a parse " +
			"failure used to mean accepted")
	}
	if !strings.Contains(err.Error(), "ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF") {
		t.Fatalf("error should point at ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF, got: %v", err)
	}
	// The message must not carry the token it just refused, which rules out
	// wrapping url.Error: its own text quotes the whole URL.
	if strings.Contains(err.Error(), "b64/token+val") {
		t.Fatalf("refusal echoes the credential: %v", err)
	}
}

// A credential whose token starts with digits and contains "/" PARSES cleanly:
// "user:12345" is a valid host:port, and the rest becomes a path, so u.User is
// nil and the old userinfo test never fired. It is the same defect on the other
// side of the parse.
func TestLoadRejectsMarketplaceGitURLWhoseCredentialLooksLikeAPort(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", "https://user:12345/tok@github.com/o/r.git")
	if _, err := Load(); err == nil {
		t.Fatal("a credential that parses as host:port plus a path was accepted")
	}
}

// An unparseable value with no credential in it is still refused rather than
// silently accepted, because "orbeat could not understand this URL" is never a
// reason to proceed with it.
func TestLoadRejectsAnUnparseableMarketplaceGitURL(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", "https://example.test:notaport/repo.git")
	_, err := Load()
	if err == nil {
		t.Fatal("an unparseable ORBEAT_MARKETPLACE_GIT_URL was accepted")
	}
	if !strings.Contains(err.Error(), "not a parseable URL") {
		t.Fatalf("want the parse refusal, got: %v", err)
	}
}

// The shapes that must keep working, restated here rather than trusted to the
// older tests, because this change widened the rule and each of these is a value
// a real deployment sets: the prod compose passes a local path, make
// smoke-remote uses git://, and SCP syntax carries a "@" that is a login, not a
// secret.
func TestLoadStillAcceptsTheLegitimateMarketplaceGitURLShapes(t *testing.T) {
	for _, raw := range []string{
		"/data/marketplace",
		"git://gitserver/marketplace.git",
		"git@github.com:org/repo.git",
		"https://example.test/repo.git",
		"file:///var/lib/orbeat/marketplace",
		"",
	} {
		t.Run("value="+raw, func(t *testing.T) {
			t.Setenv("ORBEAT_DB_URL", "postgres://x")
			t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", raw)
			if _, err := Load(); err != nil {
				t.Fatalf("Load refused a legitimate value: %v", err)
			}
		})
	}
}

// The cost of the widened rule, pinned so it is a decision on the record rather
// than a surprise: a path "@" with no credential is refused too. internal/publish
// already accepts the same over-approximation (it over-redacts the same URL), and
// the two layers now agree on one rule. If this ever needs to change, change both.
func TestLoadRefusesAPathAtSignAndThatIsTheAcceptedCost(t *testing.T) {
	t.Setenv("ORBEAT_DB_URL", "postgres://x")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_URL", "https://github.com/org/repo@v1.git")
	if _, err := Load(); err == nil {
		t.Fatal("expected the documented over-approximation to refuse a path '@'")
	}
}
