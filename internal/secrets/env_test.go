package secrets

import (
	"context"
	"testing"
)

func TestEnvProviderResolves(t *testing.T) {
	t.Setenv("ORBEAT_TEST_SECRET", "s3cr3t")
	p := EnvProvider{}
	got, err := p.Resolve(context.Background(), "ORBEAT_TEST_SECRET")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("got %q, want s3cr3t", got)
	}
}

func TestEnvProviderUnsetFailsClosed(t *testing.T) {
	p := EnvProvider{}
	if _, err := p.Resolve(context.Background(), "ORBEAT_DEFINITELY_UNSET_VAR"); err == nil {
		t.Fatal("want error for unset var, got nil")
	}
}

func TestEnvProviderEmptyLocatorFails(t *testing.T) {
	p := EnvProvider{}
	if _, err := p.Resolve(context.Background(), ""); err == nil {
		t.Fatal("want error for empty locator, got nil")
	}
}
