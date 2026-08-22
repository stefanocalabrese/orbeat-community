// Package secrets resolves catalog secret references to secret values at
// runtime. References are scheme-prefixed ("env:NAME", "vault:<mount>/<path>#<field>",
// "awssm:<name-or-arn>#<jsonkey>", later more); the DB stores references only,
// never raw secrets. Resolution is fail-closed.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrUnknownScheme is returned when a ref uses a scheme with no registered
// provider. The caller MUST treat this as fail-closed (skip the upstream).
var ErrUnknownScheme = errors.New("secrets: unknown ref scheme")

// SecretsProvider resolves the locator part of a ref (everything after the
// "<scheme>:" prefix) to a secret value, and can statically validate that
// locator's shape.
//
// ValidateLocator is deliberately MANDATORY rather than an optional
// capability-interface. As an optional interface, a future provider that simply
// forgot the method would get NO locator validation for its scheme — the feature
// quietly off for that provider, the same class of silent-misconfiguration bug as
// the unconstrained mcp_server.status. Nothing outside internal/ implements this,
// so mandatory costs nothing and the compiler enforces what a test used to guard.
type SecretsProvider interface {
	// ValidateLocator performs a static, I/O-free check that locator has the shape
	// this provider requires. Implementations MUST NOT include the locator in the
	// returned error (for an mcp_server.secret_ref it may be a raw secret pasted by
	// mistake, and the text reaches HTTP responses and logs).
	ValidateLocator(locator string) error

	Resolve(ctx context.Context, locator string) (string, error)
}

// Resolver dispatches a scheme-prefixed ref to its registered provider.
type Resolver struct {
	providers map[string]SecretsProvider
}

// NewResolver returns a Resolver with all built-in providers registered.
// Enterprise-only providers (Vault, AWS Secrets Manager) are added by
// registerEnterpriseProviders — never named here directly — so this
// constructor compiles unchanged in a generated Community tree, which has no
// VaultProvider/AWSSMProvider (docs/specs/2026-08-19-orbeat-community-
// repo-generation-design.md §4).
func NewResolver() *Resolver {
	r := &Resolver{providers: map[string]SecretsProvider{
		"env": EnvProvider{},
	}}
	registerEnterpriseProviders(r.providers)
	return r
}

// Resolve resolves ref. An empty ref yields ("", nil) — a public upstream with
// no credential. A ref without a "<scheme>:" prefix, or with an unregistered
// scheme, is an error (fail-closed).
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	scheme, locator, found := strings.Cut(ref, ":")
	if !found || scheme == "" {
		return "", fmt.Errorf("secrets: ref %q has no scheme", ref)
	}
	p, ok := r.providers[scheme]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownScheme, scheme)
	}
	return p.Resolve(ctx, locator)
}

// ValidateRef reports whether ref is well-formed and its scheme is registered,
// WITHOUT resolving it (no network, no environment lookup). An empty ref is
// valid — it means "no credential", exactly as Resolve treats it.
//
// The returned error NEVER contains ref: an mcp_server.secret_ref may hold a raw
// secret an operator pasted by mistake, and this error reaches HTTP responses and
// logs. (Resolve's own messages do echo the ref; they are log-only on the gateway
// path. Do not reuse them here.)
func (r *Resolver) ValidateRef(ref string) error {
	if ref == "" {
		return nil
	}
	// Control characters are never legitimate in any scheme's locator (env var
	// names, Vault mount/path segments, AWS names and ARNs are all printable), and
	// a NUL byte in particular reaches Postgres, which rejects NUL in a text
	// column — turning malformed input into a 500 at the DB layer instead of a 400
	// at the edge. Checked centrally so a future provider inherits it rather than
	// having to remember.
	if i := strings.IndexFunc(ref, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		return fmt.Errorf("secrets: ref contains a control character")
	}
	scheme, locator, found := strings.Cut(ref, ":")
	if !found || scheme == "" {
		return fmt.Errorf(`secrets: ref must be "<scheme>:<locator>"`)
	}
	p, ok := r.providers[scheme]
	if !ok {
		return fmt.Errorf("%w (registered: %s)", ErrUnknownScheme, strings.Join(r.Schemes(), ", "))
	}
	return p.ValidateLocator(locator)
}

// Schemes returns the registered ref schemes, sorted, so callers can render a
// deterministic "(registered: ...)" hint instead of hard-coding a list that
// would drift from NewResolver.
func (r *Resolver) Schemes() []string {
	out := make([]string, 0, len(r.providers))
	for s := range r.providers {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// lastHashCut splits s on the final '#' into (before, after, found). Vault
// mounts/paths and AWS names/ARNs never contain '#', so this cleanly separates a
// value ref from its #field selector.
func lastHashCut(s string) (before, after string, found bool) {
	i := strings.LastIndex(s, "#")
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+1:], true
}
