package gateway

import (
	"context"
	"errors"
	"testing"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/testkc"
)

// TestGatewayKeycloakVerifier proves the gateway's TokenVerifier accepts a
// genuine Keycloak access token carrying aud=orbeat-gateway and recovers the
// Principal, while rejecting garbage tokens and tokens validated against a
// different audience.
//
// It starts its OWN Keycloak container inside the test function (scoped via
// t.Cleanup) rather than a package-level TestMain, because the gateway package
// already has a TestMain in gateway_integration_test.go that starts Postgres,
// and Go permits only one TestMain per package.
func TestGatewayKeycloakVerifier(t *testing.T) {
	ctx := context.Background()

	issuer, tokenEndpoint := testkc.StartKeycloak(t, ctx)

	// Validator bound to the gateway's audience.
	validator, err := auth.NewValidator(ctx, auth.Config{Issuer: issuer, Audience: "orbeat-gateway"})
	if err != nil {
		t.Fatalf("NewValidator(orbeat-gateway): %v", err)
	}
	verifier := newTokenVerifier(validator)

	// Real access token for alice (role orbeat-user) via the password grant on
	// the public orbeat-cli client. The realm injects both orbeat-api and
	// orbeat-gateway audiences into this token (multi-audience, RFC 8707).
	rawToken := testkc.PasswordGrant(t, tokenEndpoint, "orbeat-cli", "alice", "alice")

	// Assertion 1: a genuine aud=orbeat-gateway token is accepted and yields a
	// Principal with a non-empty Subject and the orbeat-user role.
	ti, err := verifier(ctx, rawToken, nil)
	if err != nil {
		t.Fatalf("verifier rejected genuine token: %v", err)
	}
	p, ok := principalFromTokenInfo(ti)
	if !ok {
		t.Fatal("principalFromTokenInfo: no Principal in TokenInfo")
	}
	if p.Subject == "" {
		t.Fatal("Principal.Subject is empty, want non-empty")
	}
	if !containsString(p.Roles, "orbeat-user") {
		t.Fatalf("Principal.Roles = %v, want to contain orbeat-user", p.Roles)
	}

	// Assertion 2: a garbage token is rejected with an error unwrapping to
	// mcpauth.ErrInvalidToken (so the SDK returns a 401 challenge).
	if _, err := verifier(ctx, "not-a-token", nil); err == nil {
		t.Fatal("verifier accepted a garbage token, want error")
	} else if !errors.Is(err, mcpauth.ErrInvalidToken) {
		t.Fatalf("garbage-token error = %v, want errors.Is(..., mcpauth.ErrInvalidToken)", err)
	}

	// Assertion 3: a validator bound to a DIFFERENT audience rejects the SAME
	// token. This proves the aud=orbeat-gateway binding is actually enforced —
	// the gateway only accepts tokens minted for it.
	otherValidator, err := auth.NewValidator(ctx, auth.Config{Issuer: issuer, Audience: "orbeat-nonexistent"})
	if err != nil {
		t.Fatalf("NewValidator(orbeat-nonexistent): %v", err)
	}
	otherVerifier := newTokenVerifier(otherValidator)
	if _, err := otherVerifier(ctx, rawToken, nil); err == nil {
		t.Fatal("verifier bound to a different audience accepted the token, want rejection")
	} else if !errors.Is(err, mcpauth.ErrInvalidToken) {
		t.Fatalf("wrong-audience error = %v, want errors.Is(..., mcpauth.ErrInvalidToken)", err)
	}
}

// containsString reports whether ss contains want.
func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
