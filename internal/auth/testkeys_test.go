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

// authTestServer mints RS256 tokens and serves OIDC discovery + JWKS, mimicking
// Keycloak closely enough to exercise the full validation path without a container.
type authTestServer struct {
	srv              *httptest.Server
	signKey          jwk.Key // private, with kid + alg
	issuer           string  // always equals srv.URL after construction
	advertisedIssuer string  // the issuer emitted in the discovery document; defaults to issuer
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": advertised,
			// jwks_uri always points at the reachable server — the backend can always fetch it.
			"jwks_uri": ats.srv.URL + "/protocol/openid-connect/certs",
		})
	})
	mux.HandleFunc("/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		buf, _ := json.Marshal(set)
		_, _ = w.Write(buf)
	})
	ats.srv = httptest.NewServer(mux)
	ats.issuer = ats.srv.URL
	ats.advertisedIssuer = ats.srv.URL // default: discovery doc issuer == server URL
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
