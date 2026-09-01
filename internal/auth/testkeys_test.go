package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// certsPath is where authTestServer serves its JWKS, and what the default
// jwks_uri in its discovery document points at.
const certsPath = "/protocol/openid-connect/certs"

// authTestServer mints RS256 tokens and serves OIDC discovery + JWKS, mimicking
// Keycloak closely enough to exercise the full validation path without a container.
type authTestServer struct {
	srv              *httptest.Server
	signKey          jwk.Key // private, with kid + alg
	issuer           string  // always equals srv.URL after construction
	advertisedIssuer string  // the issuer emitted in the discovery document; defaults to issuer

	// advertisedJWKSURI is the jwks_uri emitted in the discovery document. It
	// defaults to this server's own certs endpoint and exists so a test can point
	// it at a DIFFERENT origin. Until audit A2 the field did not exist and
	// jwks_uri was hard-coded to srv.URL, which is why "the keys must come from
	// the discovery origin" was not an expressible property here: every test in
	// the package satisfied it by construction, so no test could fail for it.
	advertisedJWKSURI string
}

func newAuthTestServer(t *testing.T) *authTestServer {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	priv, err := jwk.Import(raw)
	if err != nil {
		t.Fatalf("import priv: %v", err)
	}
	_ = priv.Set(jwk.KeyIDKey, "test-key-1")
	_ = priv.Set(jwk.AlgorithmKey, jwa.RS256())

	pub, err := priv.PublicKey()
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	set := jwk.NewSet()
	_ = set.AddKey(pub)

	ats := &authTestServer{signKey: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		// advertisedIssuer lets tests set a distinct public issuer while keys are
		// still served from the real (reachable) server URL. Default: ats.issuer.
		advertised := ats.advertisedIssuer
		if advertised == "" {
			advertised = ats.issuer
		}
		jwks := ats.advertisedJWKSURI
		if jwks == "" {
			jwks = ats.srv.URL + certsPath
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": advertised,
			// Default: this server's own certs endpoint, so jwks_uri shares an
			// origin with the discovery URL, exactly as a real Keycloak's does.
			"jwks_uri": jwks,
		})
	})
	mux.HandleFunc(certsPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		buf, _ := json.Marshal(set)
		_, _ = w.Write(buf)
	})
	ats.srv = httptest.NewServer(mux)
	ats.issuer = ats.srv.URL
	ats.advertisedIssuer = ats.srv.URL              // default: discovery doc issuer == server URL
	ats.advertisedJWKSURI = ats.srv.URL + certsPath // default: keys on the discovery origin
	t.Cleanup(ats.srv.Close)
	return ats
}

// mint builds and signs a token with the given claims, allowing tests to set or
// omit standard claims to exercise each validation branch.
func (a *authTestServer) mint(t *testing.T, build func(b *jwt.Builder) *jwt.Builder) string {
	t.Helper()
	tok, err := build(jwt.NewBuilder()).Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), a.signKey))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return string(signed)
}

// validToken returns a token that should pass with audience "orbeat-api".
func (a *authTestServer) validToken(t *testing.T) string {
	return a.mint(t, func(b *jwt.Builder) *jwt.Builder {
		return b.
			Issuer(a.issuer).
			Subject("kc-sub-1").
			Audience([]string{"orbeat-api"}).
			Expiration(time.Now().Add(5*time.Minute)).
			IssuedAt(time.Now()).
			Claim("email", "alice@example.com").
			Claim("realm_access", map[string]any{"roles": []any{"orbeat-user"}})
	})
}
