package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
	// vs. backend http://keycloak:8080). Defaults to Issuer when empty. Same shape
	// as Issuer (no .well-known suffix).
	//
	// Two separate checks apply to the document fetched from here, and they guard
	// different things (audit A2: the first was previously documented as if it
	// covered both):
	//
	//   - issuer == Issuer catches a MISCONFIGURATION: a DiscoveryURL pointed at
	//     the wrong realm, or at a different Keycloak. It is worth nothing against
	//     a hostile discovery host, which simply echoes the string it is checked
	//     against.
	//   - jwks_uri must share an ORIGIN with DiscoveryURL (discoverJWKS). This is
	//     the check that stops a hostile discovery host, because it denies that
	//     host the ability to name someone else's signing keys as this issuer's.
	//
	// The token's iss is still enforced against Issuer on every Validate.
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
// ctx governs the background JWKS auto-refresh goroutine started by jwk.Cache.
// It must outlive the Validator: pass the application's root context, or
// another context cancelled only at process exit. Never pass a request-scoped
// context, and never pass a shutdown-signal context.
//
// Cancelling ctx does NOT degrade the Validator to serving cached keys. It
// stops httprc's controller goroutine, and Validate reads keys through
// jwk.Cache.Lookup, which sends its request on a channel that goroutine no
// longer receives from. Every subsequent Validate therefore blocks until the
// CALLER's context is done and then fails with that context's error, so a
// cancelled ctx makes the Validator serve nothing while burning each request's
// full deadline. TestValidateStallsOnceRefreshContextIsCancelled pins this,
// and cmd/gateway/main.go records at its call site why it passes a context
// that survives SIGTERM.
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
// Enforces the signature, iss, aud and exp, and rejects a token whose nbf or
// iat is set to a value outside the acceptable skew.
//
// exp is enforced as REQUIRED, not merely as checked-if-present. jwx validates
// a registered time claim's value but never its presence (jwt/validate.go's
// isExpirationValid returns nil when the claim is absent), so before
// WithRequiredClaim below a signed token minted without exp validated and
// stayed valid forever. iss and aud need no such option because WithIssuer and
// WithAudience compare values and already error on an absent claim.
//
// nbf and iat stay optional on purpose: Keycloak sends no nbf, and nothing
// here reads iat. sub is required too, enforced one layer down by
// principalFromClaims. TestValidatorRequiresExpiration pins all four choices.
func (v *Validator) Validate(ctx context.Context, raw string) (Principal, error) {
	set, err := v.cache.Lookup(ctx, v.jwksURI)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: jwks lookup: %w", err)
	}
	// "Validated" and "required" are different things here, and the sentence
	// this comment used to carry ("jwt.Parse validates registered time claims
	// (exp, nbf, iat) by default") is exactly what read as proof that exp was
	// enforced. jwt.Parse checks a registered time claim's VALUE, and only
	// when the claim is there: jwt/validate.go's isExpirationValid, isNbfValid
	// and isIssuedAtValid each return nil the moment the claim is absent.
	//
	// Required is therefore exactly three claims: exp, by WithRequiredClaim
	// below, plus iss and aud, which WithIssuer and WithAudience require as a
	// side effect of comparing values (a missing claim cannot compare equal).
	// nbf and iat are validated when present and never required; the doc
	// comment above says why each of those stays optional, and why sub is
	// enforced one layer down instead.
	//
	// WithValidate(true) is explicit intent, not enforcement: parseBytes turns
	// validation on by default, so the option only overrides a prior
	// WithValidate(false), and it enforces neither iss nor aud on its own.
	tok, err := jwt.Parse([]byte(raw),
		jwt.WithKeySet(set),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithAudience(v.cfg.Audience),
		jwt.WithRequiredClaim(jwt.ExpirationKey),
		jwt.WithAcceptableSkew(30*time.Second),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: token rejected: %w", err)
	}
	claims := tokenToClaims(tok)
	return principalFromClaims(claims)
}

// tokenToClaims builds a map[string]any from a validated jwt.Token.
// Standard claims (sub, email, preferred_username) and private claims (azp,
// realm_access) are included.
//
// This is an ALLOW-LIST, not a passthrough, so every field on Principal needs
// a copy here as well as a read in principalFromClaims. A field added in one
// place and not the other is silently always empty, and no test in
// internal/auth can catch it, because they all build the claims map directly.
// preferred_username is the current instance: it feeds Principal.IsService
// Account, and the gate for that pair is TestServiceAccountTokenIsDistinguish
// able (internal/keycloak), which parses a real Keycloak token end to end.
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
	var preferredUsername string
	if err := tok.Get("preferred_username", &preferredUsername); err == nil {
		claims["preferred_username"] = preferredUsername
	}
	var realmAccess any
	if err := tok.Get("realm_access", &realmAccess); err == nil {
		claims["realm_access"] = realmAccess
	}
	return claims
}

// discoverJWKS fetches the OIDC discovery document from discoveryBase and
// returns the jwks_uri, subject to two checks that guard different things.
//
// doc.issuer == expectedIssuer catches a MISCONFIGURATION, a discoveryBase
// pointing at the wrong realm or a different provider. It is not a security
// control, and reading it as one is what let audit A2 stand: whoever answers
// the discovery fetch composes the response, so a hostile host echoes back
// whatever issuer string it is about to be compared against, at zero cost.
//
// The security control is the origin check below. jwks_uri names the keys every
// token is then verified against, so a discovery document that may point it at
// an arbitrary host hands that host the trust anchor for the whole process: the
// audit executed exactly that, minting a token with realm role orbeat-admin
// against an attacker-held key and having this package accept it. Requiring
// jwks_uri to share an origin with discoveryBase (same scheme, host and port,
// with the scheme's default port applied when absent) removes the ability to
// redirect key material elsewhere. A real Keycloak always satisfies it: it
// composes jwks_uri from the same hostname configuration it serves discovery on.
//
// The comparison is against the CONFIGURED discoveryBase, never against
// resp.Request.URL. discoveryHTTPClient follows redirects, so a hostile host
// that captured the fetch by redirect would otherwise get to define the origin
// it is then measured against.
//
// Not gated here, and deliberately: this does NOT require https. The shipped
// production topology (deploy/docker-compose.prod.yml) fetches discovery over
// plaintext http inside the compose network, so an https requirement would
// refuse to start every existing deployment. The residual is that an attacker
// already positioned on the compose network can both answer this fetch and
// serve the JWKS from the same origin; same-origin closes the remote case, TLS
// between orbeat and Keycloak would be needed to close the on-path one.
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
	if err := sameOrigin(discoveryBase, doc.JWKSURI); err != nil {
		return "", fmt.Errorf("auth: discovery jwks_uri %q is not on the discovery origin %q: %w",
			doc.JWKSURI, discoveryBase, err)
	}
	return doc.JWKSURI, nil
}

// sameOrigin reports whether want and got carry the same web origin: scheme,
// host and port, per RFC 6454's tuple, with the scheme's default port applied
// when the URL omits it so that https://kc and https://kc:443 compare equal.
// A Keycloak configured on a non-default port emits both URLs in the same
// form, but normalizing costs nothing and a false rejection here refuses to
// start the process.
//
// Host comparison is case-insensitive because DNS names are, and url.Parse
// does not fold host case (it folds only the scheme). Hostname() is used
// rather than Host so an IPv6 literal's brackets do not enter the comparison.
func sameOrigin(want, got string) error {
	w, err := url.Parse(want)
	if err != nil {
		return fmt.Errorf("parse %q: %w", want, err)
	}
	g, err := url.Parse(got)
	if err != nil {
		return fmt.Errorf("parse %q: %w", got, err)
	}
	if !strings.EqualFold(w.Scheme, g.Scheme) {
		return fmt.Errorf("scheme %q != %q", g.Scheme, w.Scheme)
	}
	if !strings.EqualFold(w.Hostname(), g.Hostname()) {
		return fmt.Errorf("host %q != %q", g.Hostname(), w.Hostname())
	}
	if originPort(w) != originPort(g) {
		return fmt.Errorf("port %q != %q", originPort(g), originPort(w))
	}
	return nil
}

// originPort returns u's explicit port, or the default port for its scheme
// when none is given. An unknown scheme with no port yields "", which compares
// equal only to another URL of that same scheme with no port, which is the
// strict answer and the safe one.
func originPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}
