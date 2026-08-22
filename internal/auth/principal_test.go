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
