package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
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

	// wrong_signing_key and unknown_kid below are the same test twice. Both
	// sign with a freshly generated RSA key whose kid ("rogue-key", then
	// "rogue-kid-not-in-jwks") is absent from the served JWKS, so jwx's
	// key-set provider refuses both at the lookup, with "failed to find key
	// with key ID %q in key set", before any verifier is constructed. Neither
	// exercises signature verification, which is what this comment claimed
	// until 2026-08-28: between them they left the fast unit suite with zero
	// coverage of the one property they were written for.
	//
	// rogue_key_legit_kid below is the case that reaches the verifier. Both
	// of these stay, because refusing an unknown kid is a real property in
	// its own right and it is the first gate a forged token meets.
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

	// rogue_key_legit_kid is the only case in this file that reaches the
	// signature verifier. The token is signed with a freshly generated RSA
	// key, but its kid is "test-key-1", the one newAuthTestServer publishes
	// in its JWKS, so the key-set provider finds a key, hands the harness's
	// real public key to the RS256 verifier, and the check fails there with
	// "crypto/rsa: verification error".
	//
	// The assertion is on WHERE the token was refused, not on the fact that
	// it was. Every case in this function is refused, so "err != nil" passes
	// identically on a validator that never verifies a signature and merely
	// rejects an unknown kid, which is exactly how the two cases above came
	// to be read as proof of something they never touched.
	// jws.VerificationError() is the identity jwx reserves for a failed
	// cryptographic check; a kid-lookup refusal does not match it (measured),
	// so it is what separates this case from those.
	t.Run("rogue_key_legit_kid", func(t *testing.T) {
		rogueRaw, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("genkey: %v", err)
		}
		roguePriv, err := jwk.Import(rogueRaw)
		if err != nil {
			t.Fatalf("import rogue priv: %v", err)
		}
		// The kid the harness serves. Changing it to any other value turns
		// this case back into a duplicate of the two above.
		_ = roguePriv.Set(jwk.KeyIDKey, "test-key-1")
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

		_, err = v.Validate(ctx, string(signed))
		if err == nil {
			t.Fatal("a token signed by a rogue key was accepted although its kid named a key " +
				"in the JWKS: the published public key is not verifying the signature at all")
		}
		if !errors.Is(err, jws.VerificationError()) {
			t.Fatalf("the token was rejected, but not by the signature verifier: %v", err)
		}
		if strings.Contains(err.Error(), "failed to find key with key ID") {
			t.Fatalf("the token never reached a verifier: it was refused at the JWKS kid lookup, "+
				"which makes this case a duplicate of wrong_signing_key above: %v", err)
		}
	})

	// alg_none. jwa.NoSignature CAN be produced through jwx v3's high-level
	// API, with jwt.WithInsecureNoSignature() (jws.WithInsecureNoSignature()
	// is the jws-level spelling of the same thing). The comment above claimed
	// it could not until 2026-08-28, and that claim is why the oldest JWT
	// forgery of all had no case here.
	//
	// It is refused, and by which mechanism is the point: never by an
	// algorithm check. jwt.WithKeySet defaults to requireKid=true and to no
	// default key (jws's keySetProvider), so a header carrying no kid is
	// turned away at the key lookup, with `failed to find matching key: no
	// key ID ("kid") specified in token`, before any algorithm is weighed.
	//
	// Which invites the obvious follow-up, so the second half pins it rather
	// than leaving it to the reader: an unsigned token that DOES name the
	// served kid gets past the lookup and is then refused by the verifier.
	// The key the harness publishes carries alg RS256, and keySetProvider's
	// selectKey hands the verifier that KEY's algorithm without comparing the
	// token's own "alg" header, so the empty signature is checked as an RS256
	// signature and fails. "none" is never honoured as an algorithm here; it
	// is simply not what selects the verifier.
	t.Run("alg_none", func(t *testing.T) {
		tok, err := jwt.NewBuilder().
			Issuer(ats.issuer).Subject("s").Audience([]string{"orbeat-api"}).
			Expiration(time.Now().Add(5 * time.Minute)).
			IssuedAt(time.Now()).
			Build()
		if err != nil {
			t.Fatalf("build token: %v", err)
		}

		unsigned, err := jwt.Sign(tok, jwt.WithInsecureNoSignature())
		if err != nil {
			t.Fatalf("mint an alg:none token: %v", err)
		}
		_, err = v.Validate(ctx, string(unsigned))
		if err == nil {
			t.Fatal("an unsigned alg:none token was accepted; anyone can mint one")
		}
		if !strings.Contains(err.Error(), `no key ID ("kid") specified in token`) {
			t.Fatalf("the alg:none token was rejected, but not by the kid requirement this "+
				"case exists to name: %v", err)
		}
		if errors.Is(err, jws.VerificationError()) {
			t.Fatalf("the alg:none token reached a verifier; it is supposed to be turned away "+
				"one step earlier, at the key lookup: %v", err)
		}

		// Same token shape, now naming the kid the JWKS serves, so the key
		// lookup succeeds and the refusal has to come from the verifier.
		hdrs := jws.NewHeaders()
		if err := hdrs.Set(jws.KeyIDKey, "test-key-1"); err != nil {
			t.Fatalf("set kid header: %v", err)
		}
		payload, err := json.Marshal(tok)
		if err != nil {
			t.Fatalf("marshal claims: %v", err)
		}
		unsignedWithKid, err := jws.Sign(payload, jws.WithInsecureNoSignature(jws.WithProtectedHeaders(hdrs)))
		if err != nil {
			t.Fatalf("mint an alg:none token carrying a known kid: %v", err)
		}
		_, err = v.Validate(ctx, string(unsignedWithKid))
		if err == nil {
			t.Fatal("an unsigned token was accepted once it named a kid in the JWKS: its empty " +
				"signature was never checked against the published key")
		}
		if !errors.Is(err, jws.VerificationError()) {
			t.Fatalf("the unsigned token named a known kid and was rejected, but not by the "+
				"signature verifier: %v", err)
		}
	})
}

// TestValidatorDiscoveryURLDecoupledFromIssuer proves that DiscoveryURL may
// differ from Issuer: the validator fetches OIDC discovery from the server-
// reachable URL while still enforcing the browser-facing public issuer on every
// token — the exact compose scenario (localhost:8088 vs. keycloak:8080).
//
// It is also the accept half of audit A2's gate, and it is load-bearing for
// that reason: the fix requires jwks_uri to share an origin with the DISCOVERY
// URL, and a fix that compared it against Issuer instead would look correct,
// close the same attack, and break this supported configuration on every local
// dev machine and in CI. TestNewValidatorRejectsJWKSURIOnAnotherOrigin is the
// refuse half.
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

// TestValidateStallsOnceRefreshContextIsCancelled is the executable form of
// NewValidator's doc comment about its ctx parameter, which until now claimed
// the opposite of what happens: "the Validator will continue to serve cached
// keys until they expire".
//
// It serves nothing. NewValidator hands ctx to jwk.NewCache, which hands it to
// httprc's controller goroutine, and that goroutine returns on ctx.Done().
// Validate reads keys through jwk.Cache.Lookup, which sends its request on a
// channel the dead goroutine no longer receives from, so the send blocks and
// Lookup only returns once the CALLER's context is done, carrying that
// context's error. The observable consequence is not degraded validation, it
// is a request that burns its entire deadline and then fails.
//
// The assertion is on the error's identity (context.DeadlineExceeded from the
// caller's own context), not on how long anything took, so the test measures
// the mechanism and not the machine. The only clock in it is a generous
// ceiling that bounds an outright hang.
//
// A validator that really did fall back to cached keys fails here, because
// Validate would keep succeeding and never surface the caller's deadline.
func TestValidateStallsOnceRefreshContextIsCancelled(t *testing.T) {
	ats := newAuthTestServer(t)

	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	v, err := NewValidator(refreshCtx, Config{Issuer: ats.issuer, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	tok := ats.validToken(t)

	// Baseline: while the refresh context is alive the token validates, so a
	// failure after cancellation cannot be blamed on the token or the harness.
	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("Validate before cancelling the refresh context: %v", err)
	}

	cancelRefresh()

	// cancelRefresh() only marks the controller's context done; the goroutine
	// observes it on its next scheduling turn, so the very first Validate
	// after cancellation can still race in and be served. Retry until the
	// stall is observed, bounded twice over: at most 50 attempts and at most
	// 10 seconds of wall clock. Neither bound is a measurement. A validator
	// that served from cache would succeed on all 50 attempts in well under
	// the ceiling and still fail this test.
	const maxAttempts = 50
	ceiling := time.Now().Add(10 * time.Second)
	var last error
	for attempt := 0; attempt < maxAttempts && time.Now().Before(ceiling); attempt++ {
		callCtx, cancelCall := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, last = v.Validate(callCtx, tok)
		cancelCall()
		if errors.Is(last, context.DeadlineExceeded) {
			return
		}
	}
	t.Fatalf("after cancelling the refresh context, Validate never blocked until the caller's "+
		"deadline; last result was %v. Either the httprc controller outlived its context or the "+
		"Validator gained a cached-key fallback, and NewValidator's doc comment now describes "+
		"neither", last)
}

// TestValidatorRequiresExpiration pins which registered claims this validator
// insists on, and which it deliberately tolerates as absent.
//
// jwx validates a claim's VALUE but never its PRESENCE: jwt/validate.go's
// isExpirationValid, isNbfValid and isIssuedAtValid all open with
// "tv, ok := t.X(); if !ok { return nil }". iss and aud escape that because
// jwt.WithIssuer and jwt.WithAudience compare values and error on a claim
// that is not there. exp had nothing comparing it, so a signed token minted
// without exp validated, and a leaked one stayed valid forever. Nothing else
// in the stack re-checks expiry: internal/gateway snapshots entitlements
// under a max-age ceiling, not under the token's own lifetime.
//
// exp is the only claim added to the required set:
//
//   - exp is required. It is the sole bound on a token's lifetime, and
//     RFC 9068 (JWT Profile for OAuth 2.0 Access Tokens) section 2.2 lists it
//     as REQUIRED, so demanding it cannot reject a conforming access token.
//     TestKeycloakEndToEnd validates a token minted by a real Keycloak, which
//     is what keeps this from being an argument from the spec alone.
//
//   - nbf stays optional. Keycloak does not put nbf on access tokens, so
//     requiring it would reject every login. Its absence costs nothing:
//     nbf only DELAYS the start of validity, and a token with no nbf is
//     valid from issuance, which is already bounded by exp.
//
//   - iat stays optional. Requiring it would buy no enforcement, because the
//     only thing jwx does with iat is reject a token issued in the future,
//     and this validator makes no decision from iat at all. A required claim
//     nothing reads is breakage risk with no security return.
//
//   - sub is not added here because it is already enforced, one layer down:
//     principalFromClaims rejects an empty sub with "token missing sub
//     claim", so a token without it can never yield a Principal. The
//     missing_sub case below pins that, so the reason this option is absent
//     stays checked rather than merely asserted in a comment.
func TestValidatorRequiresExpiration(t *testing.T) {
	ats := newAuthTestServer(t)
	v := newValidatorForTest(t, ats)
	ctx := context.Background()

	t.Run("missing_exp_rejected", func(t *testing.T) {
		tok := ats.mint(t, func(b *jwt.Builder) *jwt.Builder {
			return b.Issuer(ats.issuer).Subject("s").Audience([]string{"orbeat-api"}).
				IssuedAt(time.Now())
		})
		_, err := v.Validate(ctx, tok)
		if err == nil {
			t.Fatal("a correctly signed token with no exp claim was accepted; it never expires, " +
				"so any copy of it is a permanent credential")
		}
		// Rejected is not enough: this token also carries no nbf, so a
		// validator that required the WRONG claim would reject it too and
		// this subtest would pass on the bug. Pin both the failure mode
		// (jwx's missing-required-claim error, whose Is() deliberately
		// ignores which claim) and the claim named in the message, which is
		// the only place jwx reports it.
		if !errors.Is(err, jwt.MissingRequiredClaimError()) {
			t.Fatalf("token without exp was rejected, but not as a missing required claim: %v", err)
		}
		if !strings.Contains(err.Error(), `required claim "exp" is missing`) {
			t.Fatalf("token without exp was rejected for a missing claim that is not exp: %v", err)
		}
	})

	t.Run("missing_nbf_accepted", func(t *testing.T) {
		tok := ats.mint(t, func(b *jwt.Builder) *jwt.Builder {
			return b.Issuer(ats.issuer).Subject("s").Audience([]string{"orbeat-api"}).
				Expiration(time.Now().Add(5 * time.Minute)).IssuedAt(time.Now())
		})
		if _, err := v.Validate(ctx, tok); err != nil {
			t.Fatalf("a token without nbf must still validate, because Keycloak does not send "+
				"one and requiring it would reject every login: %v", err)
		}
	})

	t.Run("missing_iat_accepted", func(t *testing.T) {
		tok := ats.mint(t, func(b *jwt.Builder) *jwt.Builder {
			return b.Issuer(ats.issuer).Subject("s").Audience([]string{"orbeat-api"}).
				Expiration(time.Now().Add(5 * time.Minute))
		})
		if _, err := v.Validate(ctx, tok); err != nil {
			t.Fatalf("a token without iat must still validate: iat is deliberately not in the "+
				"required set, since nothing here reads it: %v", err)
		}
	})

	t.Run("missing_sub_rejected", func(t *testing.T) {
		tok := ats.mint(t, func(b *jwt.Builder) *jwt.Builder {
			return b.Issuer(ats.issuer).Audience([]string{"orbeat-api"}).
				Expiration(time.Now().Add(5 * time.Minute)).IssuedAt(time.Now())
		})
		if _, err := v.Validate(ctx, tok); err == nil {
			t.Fatal("a token with no sub was accepted; principalFromClaims is the layer that " +
				"rejects it, and this case is why sub is not in jwt.Parse's required set")
		}
	})
}

// TestNewValidatorRejectsJWKSURIOnAnotherOrigin is the gate for audit A2: the
// discovery document's jwks_uri was trusted from any host, over any scheme.
//
// The issuer assertion one line above it is no defence. Whoever answers the
// discovery fetch composes the response, so a hostile host echoes the expected
// issuer string back and then names its own keys. This test stands the whole
// attack up: a discovery host returning the CORRECT issuer while pointing
// jwks_uri at a second, unrelated origin that holds a different signing key.
//
// It is deliberately written so that its failure message is the finding. With
// the origin check removed NewValidator succeeds, and the test does not stop
// there: it mints a token against the attacker's key carrying realm role
// orbeat-admin and reports whether this package accepted it. That is what the
// auditing agent executed, and an assertion counting only a non-nil error from
// NewValidator would read as a pass for fixes that do not close it.
func TestNewValidatorRejectsJWKSURIOnAnotherOrigin(t *testing.T) {
	// The discovery host: compromised, or simply whatever answered the name
	// ORBEAT_OIDC_DISCOVERY_URL points at inside the compose network at boot.
	discovery := newAuthTestServer(t)
	// A second origin, holding a signing key orbeat has never seen.
	attacker := newAuthTestServer(t)

	discovery.advertisedIssuer = discovery.issuer              // the correct issuer, echoed back
	discovery.advertisedJWKSURI = attacker.srv.URL + certsPath // keys from somewhere else

	v, err := NewValidator(context.Background(), Config{
		Issuer:       discovery.issuer,
		Audience:     "orbeat-api",
		DiscoveryURL: discovery.srv.URL,
	})
	if err == nil {
		forged := attacker.mint(t, func(b *jwt.Builder) *jwt.Builder {
			return b.Issuer(discovery.issuer).Subject("attacker-controlled").
				Audience([]string{"orbeat-api"}).
				Expiration(time.Now().Add(5*time.Minute)).IssuedAt(time.Now()).
				Claim("realm_access", map[string]any{"roles": []any{"orbeat-admin"}})
		})
		p, verr := v.Validate(context.Background(), forged)
		if verr == nil {
			t.Fatalf("NewValidator accepted a discovery document whose jwks_uri (%s) is on a "+
				"different origin from the discovery URL (%s), and the resulting Validator then "+
				"ACCEPTED a token signed by that origin's key: Subject=%q Roles=%v",
				discovery.advertisedJWKSURI, discovery.srv.URL, p.Subject, p.Roles)
		}
		t.Fatalf("NewValidator accepted a discovery document whose jwks_uri (%s) is on a different "+
			"origin from the discovery URL (%s); the forged token happened not to validate (%v), "+
			"but the trust anchor was still taken from the attacker's host",
			discovery.advertisedJWKSURI, discovery.srv.URL, verr)
	}
	// Fail for the right reason. Without this the test passes on any unrelated
	// startup error, which is the shape that makes a security gate vacuous.
	if !strings.Contains(err.Error(), "not on the discovery origin") {
		t.Fatalf("NewValidator failed, but not because of the jwks_uri origin: %v", err)
	}
	// The error must name BOTH, so an operator who hits this on an ordinary
	// misconfiguration can see which two origins disagreed.
	if !strings.Contains(err.Error(), discovery.advertisedJWKSURI) {
		t.Fatalf("error does not name the offending jwks_uri %q: %v", discovery.advertisedJWKSURI, err)
	}
	if !strings.Contains(err.Error(), discovery.srv.URL) {
		t.Fatalf("error does not name the discovery URL %q: %v", discovery.srv.URL, err)
	}
}

// TestSameOrigin pins the comparison itself, and the half that matters most is
// the ACCEPT half: this check runs at process start, so a false rejection does
// not weaken a boundary, it refuses to boot. The default-port cases are the
// realistic way that would happen: an issuer written without :443 beside a
// jwks_uri written with it.
func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name    string
		want    string
		got     string
		wantErr bool
	}{
		{"identical", "http://keycloak:8080/realms/orbeat", "http://keycloak:8080/realms/orbeat/protocol/openid-connect/certs", false},
		{"https default port on one side", "https://auth.example.com/realms/orbeat", "https://auth.example.com:443/realms/orbeat/certs", false},
		{"http default port on one side", "http://keycloak/realms/orbeat", "http://keycloak:80/realms/orbeat/certs", false},
		{"host case folded", "https://Auth.Example.COM/realms/orbeat", "https://auth.example.com/realms/orbeat/certs", false},
		{"different host", "http://keycloak:8080/realms/orbeat", "http://evil.example.com:8080/certs", true},
		{"different port", "http://keycloak:8080/realms/orbeat", "http://keycloak:9090/certs", true},
		{"different scheme", "https://keycloak:8080/realms/orbeat", "http://keycloak:8080/certs", true},
		// A scheme swap that keeps the effective port equal must still be
		// refused: the origin tuple includes the scheme, and http against an
		// https provider strips the transport protection entirely.
		{"scheme swap with matching explicit port", "https://kc:443/realms/orbeat", "http://kc:443/certs", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sameOrigin(tc.want, tc.got)
			if tc.wantErr && err == nil {
				t.Fatalf("sameOrigin(%q, %q) = nil; want an error", tc.want, tc.got)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("sameOrigin(%q, %q) = %v; want nil", tc.want, tc.got, err)
			}
		})
	}
}
