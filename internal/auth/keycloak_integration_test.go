package auth

import (
	"context"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/testkc"
)

// TestKeycloakEndToEnd starts a real Keycloak with the seeded orbeat realm,
// obtains a real access token for alice via the password grant on orbeat-cli,
// and asserts the validator accepts it and extracts the expected principal.
// A tampered token must be rejected.
func TestKeycloakEndToEnd(t *testing.T) {
	ctx := context.Background()

	issuer, tokenEndpoint := testkc.StartKeycloak(t, ctx)

	v, err := NewValidator(ctx, Config{Issuer: issuer, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	raw := testkc.PasswordGrant(t, tokenEndpoint, "orbeat-cli", "alice", "alice")

	p, err := v.Validate(ctx, raw)
	if err != nil {
		t.Fatalf("validate real token: %v", err)
	}
	if p.Email != "alice@example.com" {
		t.Fatalf("email = %q, want alice@example.com", p.Email)
	}
	if !containsRole(p.Roles, "orbeat-user") {
		t.Fatalf("roles = %v, want to contain orbeat-user", p.Roles)
	}
	// This is the gate that keeps "azp is the client id" a verified fact
	// rather than an assumption: a real Keycloak-issued token, not a
	// synthetic claims map.
	if p.ClientID != "orbeat-cli" {
		t.Fatalf("ClientID = %q, want orbeat-cli", p.ClientID)
	}

	// Tampered token must be rejected.
	if _, err := v.Validate(ctx, raw+"x"); err == nil {
		t.Fatal("expected tampered token to be rejected")
	}
}

// containsRole reports whether ss contains want.
func containsRole(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
