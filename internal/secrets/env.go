package secrets

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// envAllowVar names the operator knob that declares which environment
// variables an "env:" ref may name. Comma-separated PREFIXES; a trailing "*"
// on an entry is accepted and ignored, so "ORBEAT_UPSTREAM_*" and
// "ORBEAT_UPSTREAM_" mean the same thing.
const envAllowVar = "ORBEAT_SECRET_ENV_ALLOW"

// defaultEnvAllowPrefixes is what an unset (or empty) envAllowVar means. It is
// deliberately NOT "everything": an mcp_server.secret_ref is written by any
// admin over HTTP and resolved by the gateway into an Authorization header sent
// to that server's endpoint, which the same admin also chose. Without a
// constraint here, "env:ORBEAT_DB_URL" on a server pointed at an attacker's
// host mails the Postgres password (shared with Keycloak's database in
// deploy/docker-compose.prod.yml) out as a bearer token, and the audit trail
// cannot tell that write from a legitimate one (audit A4).
var defaultEnvAllowPrefixes = []string{"ORBEAT_UPSTREAM_"}

// EnvProvider resolves "env:NAME" refs from the process environment.
//
// The zero value is RESTRICTED, not permissive: it enforces
// defaultEnvAllowPrefixes. That polarity is the point: a caller that
// constructs EnvProvider{} without thinking about policy gets the safe answer,
// and only the explicit unrestricted flag (set by NewProcessConfigResolver,
// nowhere else) turns the allowlist off.
type EnvProvider struct {
	// allow is the prefix allowlist. Empty means defaultEnvAllowPrefixes; it
	// can never mean "allow everything" (see parseEnvAllow).
	allow []string
	// unrestricted disables the allowlist entirely. Set ONLY for refs that
	// come out of the process environment itself, where the supplier already
	// owns that environment and an allowlist buys nothing. See
	// NewProcessConfigResolver.
	unrestricted bool
}

// newEnvProvider builds the allowlisted provider NewResolver registers, reading
// envAllowVar once at construction.
func newEnvProvider() EnvProvider {
	return EnvProvider{allow: parseEnvAllow(os.Getenv(envAllowVar))}
}

// parseEnvAllow splits an envAllowVar value into prefixes: comma-separated,
// whitespace-trimmed, trailing "*"s dropped.
//
// Entries that are empty after that are DISCARDED rather than kept, and an
// input that yields no entries at all returns nil, which allowedPrefixes reads
// as defaultEnvAllowPrefixes. So none of "", "  ", ",,", "*", "**" or "  * , "
// produces an empty-prefix entry, and an empty prefix is exactly what
// strings.HasPrefix would treat as "match every variable". There is
// deliberately no way to spell "allow everything": the one shape an operator
// could reach for by accident is the one this fix exists to remove.
//
// TrimRight rather than TrimSuffix, so "**" collapses to nothing and falls back
// to the default. TrimSuffix left the prefix "*", which matches only variables
// whose name starts with a literal asterisk: safe (it denies everything) but
// an answer no operator would predict from what they typed.
func parseEnvAllow(raw string) []string {
	var out []string
	for _, field := range strings.Split(raw, ",") {
		p := strings.TrimRight(strings.TrimSpace(field), "*")
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// allowedPrefixes is the effective allowlist for this provider.
func (p EnvProvider) allowedPrefixes() []string {
	if len(p.allow) > 0 {
		return p.allow
	}
	return defaultEnvAllowPrefixes
}

// ValidateLocator reports whether locator is a variable name this provider may
// read: non-empty, and (unless the provider is unrestricted) carrying one of
// the allowed prefixes.
//
// This is the single chokepoint for the allowlist, and it holds because Resolve
// below calls it first: the admin write path reaches it through
// Resolver.ValidateRef and gets a 400, and the gateway's dial path reaches it
// through Resolve and fails closed, skipping that upstream. Neither can be
// satisfied while the other is not.
//
// The error names the RULE and the KNOB and never the locator. That is not a
// stylistic choice: SecretsProvider's own contract forbids echoing the locator,
// because an mcp_server.secret_ref may be a raw secret an operator pasted by
// mistake and this text reaches HTTP responses and logs. The allowed prefixes
// are operator-declared configuration, not secrets, and naming them is what
// makes the message actionable, the same trade ErrUnknownScheme already makes
// by listing the registered schemes.
//
// The VALUE receiver is load-bearing — do not "tidy" it to a pointer. NewResolver
// registers EnvProvider by value (`"env": newEnvProvider()`) while vault/awssm are
// pointers; a pointer receiver would leave the stored value outside
// SecretsProvider's method set. That is now a compile error at the NewResolver map
// literal rather than a silently skipped check, which is why the runtime guard test
// that used to protect this was retired.
func (p EnvProvider) ValidateLocator(locator string) error {
	if locator == "" {
		return fmt.Errorf("secrets/env: empty variable name")
	}
	if p.unrestricted {
		return nil
	}
	prefixes := p.allowedPrefixes()
	for _, pre := range prefixes {
		if strings.HasPrefix(locator, pre) {
			return nil
		}
	}
	return fmt.Errorf("secrets/env: variable name is not permitted by %s (allowed prefixes: %s)",
		envAllowVar, strings.Join(prefixes, ", "))
}

// Resolve returns the value of the environment variable named by locator.
// Fail-closed: an empty locator, a locator outside the allowlist, or an unset
// variable is an error.
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
