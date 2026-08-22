package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestResolverDispatchesByScheme(t *testing.T) {
	t.Setenv("ORBEAT_TEST_SECRET", "via-env")
	r := NewResolver()
	got, err := r.Resolve(context.Background(), "env:ORBEAT_TEST_SECRET")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "via-env" {
		t.Fatalf("got %q", got)
	}
}

func TestResolverEmptyRefIsEmptyNoError(t *testing.T) {
	r := NewResolver()
	got, err := r.Resolve(context.Background(), "")
	if err != nil || got != "" {
		t.Fatalf("empty ref: got %q err %v; want \"\",nil", got, err)
	}
}

func TestResolverUnknownSchemeFailsClosed(t *testing.T) {
	r := NewResolver()
	_, err := r.Resolve(context.Background(), "azurekv:secret/data/x#f")
	if !errors.Is(err, ErrUnknownScheme) {
		t.Fatalf("want ErrUnknownScheme, got %v", err)
	}
}

func TestResolverMissingSchemeFailsClosed(t *testing.T) {
	r := NewResolver()
	if _, err := r.Resolve(context.Background(), "no-scheme-here"); err == nil {
		t.Fatal("want error for ref without scheme")
	}
}

func TestResolverEnvEmptyLocatorFails(t *testing.T) {
	r := NewResolver()
	if _, err := r.Resolve(context.Background(), "env:"); err == nil {
		t.Fatal("want error for env: with empty locator")
	}
}

// TestResolverDispatchesVaultAndAWSSM moved to enterprise.ee_test.go: vault:/
// awssm: are Enterprise-only schemes, not registered by NewResolver in a
// generated Community tree (docs/specs/2026-08-19-orbeat-community-repo-
// generation-design.md §4).
