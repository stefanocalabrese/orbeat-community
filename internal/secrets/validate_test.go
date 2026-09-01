package secrets

import (
	"sort"
	"strings"
	"testing"
)

// NOTE: there is deliberately no test asserting that every registered provider
// can validate locators. ValidateLocator is a mandatory method on
// SecretsProvider, so a provider missing it — or declaring it on a pointer
// receiver while NewResolver registers a value — fails to COMPILE at the
// NewResolver map literal. A runtime test here could never fail, and a test that
// cannot fail is worse than no test.

// providerValidateLocatorCase is TestProviderValidateLocator and
// TestProviderValidateLocatorEnterprise's (internal/secrets/enterprise.ee_test.go)
// shared case shape — the latter covers vault:/awssm:, Enterprise-only
// schemes not registered in a generated Community tree.
type providerValidateLocatorCase struct {
	scheme  string
	locator string
	wantErr bool
}

// assertProviderValidateLocatorCases is the shared body both
// TestProviderValidateLocator and TestProviderValidateLocatorEnterprise run
// against their own (disjoint) scheme sets.
func assertProviderValidateLocatorCases(t *testing.T, cases []providerValidateLocatorCase) {
	t.Helper()
	r := NewResolver()
	for _, c := range cases {
		p, ok := r.providers[c.scheme]
		if !ok {
			t.Fatalf("no provider registered for scheme %q", c.scheme)
		}
		err := p.ValidateLocator(c.locator)
		if (err != nil) != c.wantErr {
			t.Errorf("%s ValidateLocator(%q) err=%v, wantErr=%v", c.scheme, c.locator, err, c.wantErr)
		}
		// A validation error must never echo the locator: for secretRef the
		// locator may be a raw secret the operator pasted by mistake.
		if err != nil && c.locator != "" && strings.Contains(err.Error(), c.locator) {
			t.Errorf("%s ValidateLocator(%q) error echoes the locator: %v", c.scheme, c.locator, err)
		}
	}
}

func TestProviderValidateLocator(t *testing.T) {
	// env: a non-empty name carrying one of ORBEAT_SECRET_ENV_ALLOW's
	// prefixes (default ORBEAT_UPSTREAM_). The allowlist itself is covered in
	// depth by env_allow_test.go; these two rows only keep the shared shape.
	assertProviderValidateLocatorCases(t, []providerValidateLocatorCase{
		{"env", "ORBEAT_UPSTREAM_TOKEN", false},
		{"env", "", true},
	})
}

// validateRefCase is TestValidateRef and TestValidateRefEnterprise's
// (internal/secrets/enterprise.ee_test.go) shared case shape.
type validateRefCase struct {
	ref     string
	wantErr bool
}

// assertValidateRefCases is the shared body both TestValidateRef and
// TestValidateRefEnterprise run against their own (disjoint) ref sets.
func assertValidateRefCases(t *testing.T, cases []validateRefCase) {
	t.Helper()
	r := NewResolver()
	for _, c := range cases {
		err := r.ValidateRef(c.ref)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateRef(%q) err=%v, wantErr=%v", c.ref, err, c.wantErr)
		}
		// A validation error must never echo operator-supplied material — but WHICH
		// part is operator-supplied depends on the ref's shape:
		//
		//   - With a scheme ("vault:kv/mcp#"), the scheme is a fixed public token
		//     that provider messages legitimately name ("secrets/vault: ..."), and
		//     the LOCATOR is the sensitive part. Asserting on the whole ref would
		//     false-positive on a bare "vault:", whose own locator-free error
		//     contains "vault:" via that prefix.
		//   - With no usable scheme ("ghp_rawsecret"), the ENTIRE ref is
		//     operator-supplied — the pasted-raw-secret case — so none of it may
		//     appear.
		if err != nil {
			scheme, locator, found := strings.Cut(c.ref, ":")
			switch {
			case !found || scheme == "":
				if c.ref != "" && strings.Contains(err.Error(), c.ref) {
					t.Errorf("ValidateRef(%q) error echoes the ref: %v", c.ref, err)
				}
			case locator != "":
				if strings.Contains(err.Error(), locator) {
					t.Errorf("ValidateRef(%q) error echoes the locator %q: %v", c.ref, locator, err)
				}
			}
		}
	}
}

func TestValidateRef(t *testing.T) {
	assertValidateRefCases(t, []validateRefCase{
		{"", false}, // empty == "no credential", matching Resolve
		{"env:ORBEAT_UPSTREAM_TOKEN", false},

		{"TOKEN", true},              // no scheme prefix
		{"valut:kv/mcp#token", true}, // unregistered scheme (a typo of "vault", never registered in either tier)
		{":TOKEN", true},             // empty scheme
		{"env:", true},               // empty locator
		// Control characters are never legitimate in a ref: a NUL byte reaches
		// Postgres, which rejects NUL in a text column, so an unchecked ref turns
		// malformed input into a 500 at the DB layer instead of a 400 at the edge.
		{"env:ORBEAT_UPSTREAM_A\x00B", true},
		{"env:ORBEAT_UPSTREAM_A\nB", true},
	})
}

// Validation is shape-checking, not resolution: a well-formed ref whose target
// does not exist must still validate. If this ever fails, ValidateRef has
// started doing I/O.
func TestValidateRefDoesNotResolve(t *testing.T) {
	if err := NewResolver().ValidateRef("env:ORBEAT_UPSTREAM_DEFINITELY_NOT_SET_12345"); err != nil {
		t.Errorf("ValidateRef on a well-formed ref to a missing target = %v, want nil", err)
	}
}

// TestSchemesSorted asserts against an expectation built from "env" plus
// enterpriseSchemes() — not a hard-coded tier-specific literal — so this
// shared test asserts the right thing in both the Enterprise build (which
// registers vault/awssm) and a generated Community tree (which does not).
func TestSchemesSorted(t *testing.T) {
	got := NewResolver().Schemes()
	want := append([]string{"env"}, enterpriseSchemes()...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("Schemes() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Schemes() = %v, want %v (sorted)", got, want)
		}
	}
}
