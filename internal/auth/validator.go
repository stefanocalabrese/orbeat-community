package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lestrrat-go/httprc/v3"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// discoveryHTTPClient is used for the one-shot OIDC discovery fetch in
// discoverJWKS (the JWKS auto-refresh started afterwards uses its own httprc
// client, unaffected by this). A dedicated timeout closes the exact defect
// class fixed in orbeat-sync v1.16.0 ("could HANG FOREVER" — http.DefaultClient
// has none): a Keycloak that accepts the TCP connection but never responds
// would otherwise hang orbeat-api/orbeat-gateway startup indefinitely, with no
// error and no health signal (audit B5). A package-level var, not a const,
// so tests can inject a short timeout instead of waiting out 15 real seconds.
var discoveryHTTPClient = &http.Client{Timeout: 15 * time.Second}

// maxDiscoveryBodyBytes bounds the discovery document read (audit B5: the body
// read was previously unbounded).
const maxDiscoveryBodyBytes = 1 << 20

// Config configures a Validator.
type Config struct {
	Issuer   string // expected token issuer, e.g. http://kc/realms/orbeat
	Audience string // this resource server's audience, e.g. orbeat-api

	// DiscoveryURL is an optional server-reachable base URL for OIDC discovery +
	// JWKS fetch, used when the public issuer URL (in tokens) differs from the URL
	// this resource server can reach (e.g. browser/token issuer http://localhost:8088
	// vs. backend http://keycloak:8080). Defaults to Issuer when empty. The discovery
	// document fetched here MUST still advertise issuer == Issuer (asserted), and the
	// token's iss is still enforced against Issuer. Same shape as Issuer (no .well-known suffix).
	DiscoveryURL string
}

// Validator validates OAuth 2.1 access tokens against an OIDC provider's JWKS.
type Validator struct {
	cfg     Config
	jwksURI string
	cache   *jwk.Cache
}

// NewValidator performs OIDC discovery and starts an auto-refreshing JWKS cache.
//
// ctx governs the background JWKS auto-refresh goroutines started by jwk.Cache.
// It must remain alive for the entire lifetime of the Validator — pass the
// application's root context (or another long-lived context), never a
// request-scoped one. Cancelling ctx stops key refresh; the Validator will
// continue to serve cached keys until they expire, after which signature
// verification will fail for tokens signed with rotated keys.
func NewValidator(ctx context.Context, cfg Config) (*Validator, error) {
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, fmt.Errorf("auth: Issuer and Audience are required")
	}
	discoveryBase := cfg.DiscoveryURL
	if discoveryBase == "" {
		discoveryBase = cfg.Issuer
	}
	jwksURI, err := discoverJWKS(ctx, discoveryBase, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	// httprc.NewClient() constructs the HTTP refresh client required by jwk.NewCache.
	// The cache is auto-refreshing; Register with WaitReady=true (default) performs
	// the initial fetch synchronously, equivalent to the plan's Refresh() call.
	client := httprc.NewClient()
	cache, err := jwk.NewCache(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("auth: new jwk cache: %w", err)
	}
	if err := cache.Register(ctx, jwksURI); err != nil {
		return nil, fmt.Errorf("auth: register jwks: %w", err)
	}
	return &Validator{cfg: cfg, jwksURI: jwksURI, cache: cache}, nil
}

// Validate parses and fully validates raw, returning the extracted Principal.
// Enforces signature, issuer, audience, and expiry/nbf — none skipped.
func (v *Validator) Validate(ctx context.Context, raw string) (Principal, error) {
	set, err := v.cache.Lookup(ctx, v.jwksURI)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: jwks lookup: %w", err)
	}
	// jwt.Parse validates registered time claims (exp, nbf, iat) by default.
	// WithValidate(true) is explicit intent — it overrides any prior WithValidate(false)
	// and is otherwise a no-op; it does NOT by itself enforce iss or aud.
	// WithIssuer and WithAudience are what actually enforce those claims.
	tok, err := jwt.Parse([]byte(raw),
		jwt.WithKeySet(set),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithAudience(v.cfg.Audience),
		jwt.WithAcceptableSkew(30*time.Second),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: token rejected: %w", err)
	}
	claims := tokenToClaims(tok)
	return principalFromClaims(claims)
}

// tokenToClaims builds a map[string]any from a validated jwt.Token.
// Standard claims (sub, email) and private claims (azp, realm_access) are included.
func tokenToClaims(tok jwt.Token) map[string]any {
	claims := make(map[string]any)
	if sub, ok := tok.Subject(); ok {
		claims["sub"] = sub
	}
	var email string
	if err := tok.Get("email", &email); err == nil {
		claims["email"] = email
	}
	var azp string
	if err := tok.Get("azp", &azp); err == nil {
		claims["azp"] = azp
	}
	var realmAccess any
	if err := tok.Get("realm_access", &realmAccess); err == nil {
		claims["realm_access"] = realmAccess
	}
	return claims
}

// discoverJWKS fetches the OIDC discovery document from discoveryBase and
// returns the jwks_uri. It asserts that the fetched document's issuer field
// equals expectedIssuer — the ONLY change from the original is that the fetch
// URL (discoveryBase) may differ from the expected issuer (expectedIssuer).
// When they are equal (DiscoveryURL not set), behaviour is identical to before.
func discoverJWKS(ctx context.Context, discoveryBase, expectedIssuer string) (string, error) {
	url := discoveryBase + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("auth: discovery request: %w", err)
	}
	resp, err := discoveryHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth: discovery fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth: discovery status %d", resp.StatusCode)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDiscoveryBodyBytes)).Decode(&doc); err != nil {
		return "", fmt.Errorf("auth: decode discovery: %w", err)
	}
	if doc.Issuer != expectedIssuer {
		return "", fmt.Errorf("auth: discovery issuer %q != configured %q", doc.Issuer, expectedIssuer)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("auth: discovery missing jwks_uri")
	}
	return doc.JWKSURI, nil
}
