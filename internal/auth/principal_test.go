package auth

import (
	"reflect"
	"sort"
	"testing"
)

func TestPrincipalFromClaims(t *testing.T) {
	claims := map[string]any{
		"sub":   "kc-uuid-123",
		"email": "alice@example.com",
		"realm_access": map[string]any{
			"roles": []any{"orbeat-user", "orbeat-admin"},
		},
	}
	p, err := principalFromClaims(claims)
	if err != nil {
		t.Fatalf("principalFromClaims: %v", err)
	}
	if p.Subject != "kc-uuid-123" {
		t.Fatalf("Subject = %q", p.Subject)
	}
	if p.Email != "alice@example.com" {
		t.Fatalf("Email = %q", p.Email)
	}
	got := append([]string(nil), p.Roles...)
	sort.Strings(got)
	want := []string{"orbeat-admin", "orbeat-user"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Roles = %v, want %v", got, want)
	}
}

func TestPrincipalRequiresSubject(t *testing.T) {
	if _, err := principalFromClaims(map[string]any{"email": "x@y.z"}); err == nil {
		t.Fatal("expected error when sub is missing")
	}
}

func TestPrincipalNoRoles(t *testing.T) {
	p, err := principalFromClaims(map[string]any{"sub": "s"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Roles) != 0 {
		t.Fatalf("expected no roles, got %v", p.Roles)
	}
}

func TestPrincipalFromClaimsReadsAzp(t *testing.T) {
	p, err := principalFromClaims(map[string]any{"sub": "u1", "azp": "orbeat-cli"})
	if err != nil {
		t.Fatalf("principalFromClaims: %v", err)
	}
	if p.ClientID != "orbeat-cli" {
		t.Errorf("ClientID = %q, want %q", p.ClientID, "orbeat-cli")
	}
}

func TestPrincipalFromClaimsToleratesMissingAzp(t *testing.T) {
	p, err := principalFromClaims(map[string]any{"sub": "u1"})
	if err != nil {
		t.Fatalf("principalFromClaims: %v", err)
	}
	if p.ClientID != "" {
		t.Errorf("ClientID = %q, want empty", p.ClientID)
	}
}

// TestPrincipalFromClaimsReadsPreferredUsername pins the second half of the
// pair IsServiceAccount depends on. Dropping the preferred_username read
// leaves the predicate permanently false, which is the silent direction: a
// deleted virtual key goes back to resolving as a person and nothing in
// internal/gateway fails, because a false answer there means "fall through",
// which is what it did before the refusal existed.
func TestPrincipalFromClaimsReadsPreferredUsername(t *testing.T) {
	p, err := principalFromClaims(map[string]any{"sub": "u1", "preferred_username": "alice"})
	if err != nil {
		t.Fatalf("principalFromClaims: %v", err)
	}
	if p.PreferredUsername != "alice" {
		t.Errorf("PreferredUsername = %q, want %q", p.PreferredUsername, "alice")
	}
	if q, err := principalFromClaims(map[string]any{"sub": "u1"}); err != nil {
		t.Fatalf("principalFromClaims: %v", err)
	} else if q.PreferredUsername != "" {
		t.Errorf("PreferredUsername = %q with the claim absent, want empty", q.PreferredUsername)
	}
}

// TestIsServiceAccount pins the predicate the gateway refuses an orphaned
// robot on. The cases that matter are not the happy one: they are the four
// NEGATIVES, because a false positive here refuses a human and breaks every
// login on the deployment, while a false negative merely leaves the old
// fall-through in place.
//
// Two of them exist specifically to kill the obvious cheaper implementation,
// strings.HasPrefix(p.PreferredUsername, "service-account-"): a person
// genuinely named that, and a service account presenting ANOTHER client's
// username. Both pass a prefix test and both must be refused by an exact
// match against this token's own azp. The mutant is that one-line swap and
// it fails on both.
func TestIsServiceAccount(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		want bool
	}{
		{
			// The measured Keycloak shape: see serviceAccountUsernamePrefix.
			"keycloak service account",
			Principal{
				Subject:           "fd191726-634d-4d3f-8dc4-6f6900f368df",
				ClientID:          "61062fd7-55aa-460e-8437-1d90941979fb",
				PreferredUsername: "service-account-61062fd7-55aa-460e-8437-1d90941979fb",
			},
			true,
		},
		{
			// Keycloak lowercases usernames, so a mixed-case client id still
			// has to match. EqualFold, not ==.
			"client id case differs from the username",
			Principal{ClientID: "Robot-CI", PreferredUsername: "service-account-robot-ci"},
			true,
		},
		{
			"an ordinary human",
			Principal{Subject: "41598ac1", ClientID: "orbeat-cli", PreferredUsername: "alice"},
			false,
		},
		{
			// A prefix check says true here and refuses a real person.
			"a human whose username merely starts with the prefix",
			Principal{ClientID: "orbeat-cli", PreferredUsername: "service-account-manager"},
			false,
		},
		{
			// A prefix check says true here too. The username names a
			// different client than the one this token was issued to, which
			// proves nothing about this token.
			"service-account username for a different client",
			Principal{ClientID: "orbeat-cli", PreferredUsername: "service-account-some-other-client"},
			false,
		},
		{
			// A non-Keycloak IdP, or any token without the claim. Answering
			// false is the documented, safe degradation.
			"no preferred_username at all",
			Principal{ClientID: "61062fd7-55aa-460e-8437-1d90941979fb"},
			false,
		},
		{
			"no azp at all",
			Principal{PreferredUsername: "service-account-"},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.IsServiceAccount(); got != c.want {
				t.Errorf("IsServiceAccount() = %v, want %v (ClientID=%q PreferredUsername=%q)",
					got, c.want, c.p.ClientID, c.p.PreferredUsername)
			}
		})
	}
}
