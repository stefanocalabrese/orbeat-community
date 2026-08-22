package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

func newValidatorForTest(t *testing.T, ats *authTestServer) *Validator {
	t.Helper()
	v, err := NewValidator(context.Background(), Config{
		Issuer:   ats.issuer,
		Audience: "orbeat-api",
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

func TestValidatorAcceptsValidToken(t *testing.T) {
	ats := newAuthTestServer(t)
	v := newValidatorForTest(t, ats)

	p, err := v.Validate(context.Background(), ats.validToken(t))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p.Subject != "kc-sub-1" || p.Email != "alice@example.com" {
		t.Fatalf("principal = %+v", p)
	}
	if len(p.Roles) != 1 || p.Roles[0] != "orbeat-user" {
		t.Fatalf("roles = %v", p.Roles)
	}
}

func TestValidatorRejects(t *testing.T) {
	ats := newAuthTestServer(t)
	v := newValidatorForTest(t, ats)
	ctx := context.Background()

	t.Run("expired", func(t *testing.T) {
		tok := ats.mint(t, func(b *jwt.Builder) *jwt.Builder {
			return b.Issuer(ats.issuer).Subject("s").Audience([]string{"orbeat-api"}).
				Expiration(time.Now().Add(-1 * time.Minute)).IssuedAt(time.Now().Add(-2 * time.Minute))
		})
		if _, err := v.Validate(ctx, tok); err == nil {
			t.Fatal("expected expired token to be rejected")
		}
	})
	t.Run("wrong audience", func(t *testing.T) {
		tok := ats.mint(t, func(b *jwt.Builder) *jwt.Builder {
			return b.Issuer(ats.issuer).Subject("s").Audience([]string{"someone-else"}).
				Expiration(time.Now().Add(5 * time.Minute)).IssuedAt(time.Now())
		})
		if _, err := v.Validate(ctx, tok); err == nil {
			t.Fatal("expected wrong-audience token to be rejected")
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		tok := ats.mint(t, func(b *jwt.Builder) *jwt.Builder {
			return b.Issuer("https://evil.example").Subject("s").Audience([]string{"orbeat-api"}).
				Expiration(time.Now().Add(5 * time.Minute)).IssuedAt(time.Now())
		})
		if _, err := v.Validate(ctx, tok); err == nil {
			t.Fatal("expected wrong-issuer token to be rejected")
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, err := v.Validate(ctx, "not-a-jwt"); err == nil {
			t.Fatal("expected malformed token to be rejected")
		}
	})

	// nbf in the future: token is structurally valid and correctly signed but
	// NotBefore is set 5 minutes in the future, well beyond the 30s acceptable
	// skew configured in the validator. Must be rejected.
	t.Run("nbf_in_future", func(t *testing.T) {
		tok := ats.mint(t, func(b *jwt.Builder) *jwt.Builder {
			return b.Issuer(ats.issuer).Subject("s").Audience([]string{"orbeat-api"}).
				Expiration(time.Now().Add(10 * time.Minute)).
				IssuedAt(time.Now()).
				NotBefore(time.Now().Add(5 * time.Minute))
		})
		if _, err := v.Validate(ctx, tok); err == nil {
			t.Fatal("expected nbf-in-future token to be rejected")
		}
	})

	// wrong_signing_key: token is signed with a freshly-generated RSA key whose
	// public key is NOT in the served JWKS. This proves that signature verification
	// is enforced — no rogue key can be trusted. (jwa.NoSignature cannot be
	// produced via the jwx v3 high-level API; a rogue-key approach gives the
	// same assurance: only keys in the JWKS are accepted.)
	t.Run("wrong_signing_key", func(t *testing.T) {
		rogueRaw, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("genkey: %v", err)
		}
		roguePriv, err := jwk.Import(rogueRaw)
		if err != nil {
			t.Fatalf("import rogue priv: %v", err)
		}
		_ = roguePriv.Set(jwk.KeyIDKey, "rogue-key")
		_ = roguePriv.Set(jwk.AlgorithmKey, jwa.RS256())

		tok, err := jwt.NewBuilder().
			Issuer(ats.issuer).Subject("s").Audience([]string{"orbeat-api"}).
			Expiration(time.Now().Add(5 * time.Minute)).
			IssuedAt(time.Now()).
			Build()
		if err != nil {
			t.Fatalf("build token: %v", err)
		}
		signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), roguePriv))
		if err != nil {
			t.Fatalf("sign with rogue key: %v", err)
		}
		if _, err := v.Validate(ctx, string(signed)); err == nil {
			t.Fatal("expected token signed by unknown key to be rejected")
		}
	})

	// unknown_kid: token is signed with a key whose kid is not present in the
	// served JWKS. The JWKS lookup must fail to match any key and reject the token.
	t.Run("unknown_kid", func(t *testing.T) {
		rogueRaw, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("genkey: %v", err)
		}
		roguePriv, err := jwk.Import(rogueRaw)
		if err != nil {
			t.Fatalf("import rogue priv: %v", err)
		}
		_ = roguePriv.Set(jwk.KeyIDKey, "rogue-kid-not-in-jwks")
		_ = roguePriv.Set(jwk.AlgorithmKey, jwa.RS256())

		tok, err := jwt.NewBuilder().
			Issuer(ats.issuer).Subject("s").Audience([]string{"orbeat-api"}).
			Expiration(time.Now().Add(5 * time.Minute)).
			IssuedAt(time.Now()).
			Build()
		if err != nil {
			t.Fatalf("build token: %v", err)
		}
		signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), roguePriv))
		if err != nil {
			t.Fatalf("sign with rogue kid: %v", err)
		}
		if _, err := v.Validate(ctx, string(signed)); err == nil {
			t.Fatal("expected token with unknown kid to be rejected")
		}
	})
}

// TestValidatorDiscoveryURLDecoupledFromIssuer proves that DiscoveryURL may
// differ from Issuer: the validator fetches OIDC discovery from the server-
// reachable URL while still enforcing the browser-facing public issuer on every
// token — the exact compose scenario (localhost:8088 vs. keycloak:8080).
func TestValidatorDiscoveryURLDecoupledFromIssuer(t *testing.T) {
	ats := newAuthTestServer(t)
	const publicIssuer = "https://public.example/realms/orbeat"
	ats.advertisedIssuer = publicIssuer // discovery doc (at ats.srv.URL) claims this issuer

	v, err := NewValidator(context.Background(), Config{
		Issuer:       publicIssuer, // token iss we trust (browser-facing)
		Audience:     "orbeat-api",
		DiscoveryURL: ats.srv.URL, // server-reachable URL we actually fetch from
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// A token whose iss matches the public issuer, signed by the server's key, must validate.
	tok := ats.mint(t, func(b *jwt.Builder) *jwt.Builder {
		return b.Issuer(publicIssuer).Subject("kc-sub-1").Audience([]string{"orbeat-api"}).
			Expiration(time.Now().Add(5 * time.Minute)).IssuedAt(time.Now())
	})
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("expected decoupled token to validate: %v", err)
	}

	// A token with the WRONG iss (the server URL, not the public issuer) must be
	// rejected — the iss enforcement is unchanged even though discovery is fetched
	// from a different URL.
	bad := ats.mint(t, func(b *jwt.Builder) *jwt.Builder {
		return b.Issuer(ats.srv.URL).Subject("s").Audience([]string{"orbeat-api"}).
			Expiration(time.Now().Add(5 * time.Minute)).IssuedAt(time.Now())
	})
	if _, err := v.Validate(context.Background(), bad); err == nil {
		t.Fatal("expected token with wrong iss to be rejected even with decoupled discovery")
	}
}

// TestNewValidatorRejectsIssuerMismatchInDiscovery asserts that NewValidator
// fails when the discovery document's issuer field does not match the configured
// Issuer — a security invariant that prevents a rogue endpoint from serving a
// valid-looking discovery doc for a different issuer.
func TestNewValidatorRejectsIssuerMismatchInDiscovery(t *testing.T) {
	ats := newAuthTestServer(t)
	ats.advertisedIssuer = "https://mismatch.example/realms/other"

	_, err := NewValidator(context.Background(), Config{
		Issuer:       "https://different.example/realms/orbeat",
		Audience:     "orbeat-api",
		DiscoveryURL: ats.srv.URL,
	})
	if err == nil {
		t.Fatal("expected NewValidator to fail when discovery issuer != configured issuer")
	}
}

// TestNewValidatorTimesOutOnHangingDiscovery proves discoverJWKS no longer
// hangs forever against a TCP-accepting-but-non-responding discovery endpoint
// (audit B5 — the exact defect class v1.16.0 fixed in orbeat-sync's HTTP
// client, still present here). discoveryHTTPClient is a package-level var
// specifically so a test can inject a short timeout instead of waiting out the
// real 15s production value.
func TestNewValidatorTimesOutOnHangingDiscovery(t *testing.T) {
	orig := discoveryHTTPClient
	discoveryHTTPClient = &http.Client{Timeout: 30 * time.Millisecond}
	t.Cleanup(func() { discoveryHTTPClient = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // far longer than the injected 30ms timeout
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	_, err := NewValidator(context.Background(), Config{
		Issuer:   srv.URL,
		Audience: "orbeat-api",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected NewValidator to fail against a hanging discovery server")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("NewValidator took %v — did not time out promptly", elapsed)
	}
}

// TestDiscoverJWKSLimitsResponseBodySize proves the discovery fetch reads at
// most 1 MiB of response body (audit B5). The discovery document places a
// >1MiB padding field BEFORE jwks_uri: without the cap, the whole (valid) JSON
// object decodes successfully; with io.LimitReader capping the read, the
// stream is cut mid-padding, the JSON is left syntactically incomplete, and
// decode fails — isolating the size-cap behavior from any other failure mode
// (discoverJWKS never makes a second network call on success, so there is
// nothing else that could fail).
func TestDiscoverJWKSLimitsResponseBodySize(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		padding := strings.Repeat("x", 2<<20) // 2 MiB — pushes jwks_uri past the 1 MiB cap
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"padding":%q,"jwks_uri":"https://example.invalid/certs"}`, srv.URL, padding)
	}))
	t.Cleanup(srv.Close)

	if _, err := discoverJWKS(context.Background(), srv.URL, srv.URL); err == nil {
		t.Fatal("expected discoverJWKS to fail when the discovery document exceeds the 1 MiB read cap")
	}
}
