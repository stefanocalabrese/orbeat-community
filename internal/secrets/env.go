package secrets

import (
	"context"
	"fmt"
	"os"
)

// EnvProvider resolves "env:NAME" refs from the process environment.
type EnvProvider struct{}

// ValidateLocator reports whether locator is a usable variable name.
//
// The VALUE receiver is load-bearing — do not "tidy" it to a pointer. NewResolver
// registers EnvProvider by value (`"env": EnvProvider{}`) while vault/awssm are
// pointers; a pointer receiver would leave the stored value outside
// SecretsProvider's method set. That is now a compile error at the NewResolver map
// literal rather than a silently skipped check, which is why the runtime guard test
// that used to protect this was retired.
func (EnvProvider) ValidateLocator(locator string) error {
	if locator == "" {
		return fmt.Errorf("secrets/env: empty variable name")
	}
	return nil
}

// Resolve returns the value of the environment variable named by locator.
// Fail-closed: an empty locator or unset variable is an error.
func (p EnvProvider) Resolve(_ context.Context, locator string) (string, error) {
	if err := p.ValidateLocator(locator); err != nil {
		return "", err
	}
	v, ok := os.LookupEnv(locator)
	if !ok {
		return "", fmt.Errorf("secrets/env: %q is not set", locator)
	}
	if v == "" {
		return "", fmt.Errorf("secrets/env: %q is set but empty", locator)
	}
	return v, nil
}
