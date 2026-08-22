// Package syncclient implements the orbeat-sync CLI: device-flow auth, the
// /v1/sync/artifacts API client, and a manifest-bounded reconcile into ~/.claude.
package syncclient

import "os"

// Config holds the orbeat-sync CLI settings (ORBEAT_-prefixed env, with defaults).
type Config struct {
	APIBaseURL    string // orbeat-api base, e.g. http://localhost:8080
	OIDCIssuer    string // realm issuer, e.g. http://localhost:8088/realms/orbeat
	OIDCDiscovery string // discovery base; defaults to OIDCIssuer when empty
	ClientID      string // OAuth client id (default orbeat-cli)
}

// LoadConfig reads configuration from the environment, applying defaults.
func LoadConfig() Config {
	c := Config{
		APIBaseURL:    getenv("ORBEAT_API_URL", "http://localhost:8080"),
		OIDCIssuer:    getenv("ORBEAT_OIDC_ISSUER", "http://localhost:8088/realms/orbeat"),
		OIDCDiscovery: os.Getenv("ORBEAT_OIDC_DISCOVERY_URL"),
		ClientID:      getenv("ORBEAT_SYNC_CLIENT_ID", "orbeat-cli"),
	}
	if c.OIDCDiscovery == "" {
		c.OIDCDiscovery = c.OIDCIssuer
	}
	return c
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
