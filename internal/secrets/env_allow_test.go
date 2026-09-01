package secrets

import (
	"context"
	"strings"
	"testing"
)

// dbURLVar is the variable audit A4's attack names. It is not special to the
// code, since any name outside the allowlist behaves identically, but using the
// real one keeps the tests pointed at the scenario they exist for: an admin
// writing secretRef "env:ORBEAT_DB_URL" on a server whose endpoint they also
// chose, and the gateway mailing the Postgres password out as a bearer token.
const dbURLVar = "ORBEAT_DB_URL"

// TestEnvAllowDefaultAppliesWhenUnset is the fail-closed half at WRITE time:
// with ORBEAT_SECRET_ENV_ALLOW unset, the default ORBEAT_UPSTREAM_ prefix is in
// force, so ValidateRef refuses the ref before it can ever be stored. The api
// layer turns this into a 400 (TestServerWriteRejectsDisallowedEnvRef).
func TestEnvAllowDefaultAppliesWhenUnset(t *testing.T) {
	t.Setenv(envAllowVar, "")
	r := NewResolver()
	if err := r.ValidateRef("env:" + dbURLVar); err == nil {
		t.Fatalf("ValidateRef(env:%s) = nil with %s unset; the default allowlist did not apply", dbURLVar, envAllowVar)
	}
	if err := r.ValidateRef("env:ORBEAT_UPSTREAM_GITHUB_TOKEN"); err != nil {
		t.Fatalf("ValidateRef on an ORBEAT_UPSTREAM_ name = %v, want nil", err)
	}
}

// TestEnvAllowResolveFailsClosedEvenWhenTheVariableIsSet is the fail-closed
// half at DIAL time, and it is a separate assertion from the write-time one on
// purpose: a row written before this release already holds "env:ORBEAT_DB_URL",
// so refusing new writes closes nothing on its own. The variable is deliberately
// SET here: a test against an unset variable would pass on a build with no
// allowlist at all, since Resolve errors on "is not set" either way. That is the
// mutant this row exists to kill.
func TestEnvAllowResolveFailsClosedEvenWhenTheVariableIsSet(t *testing.T) {
	t.Setenv(envAllowVar, "")
	t.Setenv(dbURLVar, "postgres://orbeat:hunter2@postgres:5432/orbeat")
	r := NewResolver()
	got, err := r.Resolve(context.Background(), "env:"+dbURLVar)
	if err == nil {
		t.Fatalf("Resolve(env:%s) returned a value with the variable set; the allowlist is not enforced on the resolve path", dbURLVar)
	}
	if got != "" {
		t.Fatalf("Resolve returned %q alongside its error; it must return no value", got)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("resolve error leaked the value: %v", err)
	}
}

// TestEnvAllowHonoursAnOperatorList pins that the knob actually widens the
// rule, and that widening it does NOT implicitly re-admit the default.
func TestEnvAllowHonoursAnOperatorList(t *testing.T) {
	t.Setenv(envAllowVar, "ACME_MCP_, ORBEAT_UPSTREAM_")
	r := NewResolver()
	for _, ok := range []string{"ACME_MCP_TOKEN", "ORBEAT_UPSTREAM_TOKEN"} {
		if err := r.ValidateRef("env:" + ok); err != nil {
			t.Errorf("ValidateRef(env:%s) = %v, want nil", ok, err)
		}
	}
	if err := r.ValidateRef("env:" + dbURLVar); err == nil {
		t.Errorf("ValidateRef(env:%s) = nil under an explicit list that does not cover it", dbURLVar)
	}

	// A list that replaces the default really replaces it.
	t.Setenv(envAllowVar, "ACME_MCP_")
	if err := NewResolver().ValidateRef("env:ORBEAT_UPSTREAM_TOKEN"); err == nil {
		t.Error("an explicit list that omits ORBEAT_UPSTREAM_ still admitted it; the default is being merged in")
	}
}

// TestEnvAllowCannotBeWidenedToEverything is the decision this fix has to pin:
// there is no spelling of ORBEAT_SECRET_ENV_ALLOW that means "any variable".
// Every input here yields no usable prefix, and each must fall back to the
// default rather than to an empty prefix, which strings.HasPrefix matches
// against every name.
func TestEnvAllowCannotBeWidenedToEverything(t *testing.T) {
	for _, raw := range []string{"", "   ", ",", ",,,", "*", " * ", ", ,", "**"} {
		t.Run("value="+raw, func(t *testing.T) {
			t.Setenv(envAllowVar, raw)
			r := NewResolver()
			if err := r.ValidateRef("env:" + dbURLVar); err == nil {
				t.Fatalf("%s=%q admitted env:%s; it must fall back to the default, never to allow-everything",
					envAllowVar, raw, dbURLVar)
			}
			if err := r.ValidateRef("env:ORBEAT_UPSTREAM_TOKEN"); err != nil {
				t.Fatalf("%s=%q did not fall back to the default: %v", envAllowVar, raw, err)
			}
		})
	}
}

// A trailing "*" is accepted so an operator can write the glob form the docs
// use, and it means exactly the same prefix as writing it without.
func TestEnvAllowAcceptsTrailingStar(t *testing.T) {
	t.Setenv(envAllowVar, "ACME_MCP_*")
	r := NewResolver()
	if err := r.ValidateRef("env:ACME_MCP_TOKEN"); err != nil {
		t.Fatalf("ValidateRef under a trailing-star prefix = %v, want nil", err)
	}
	if err := r.ValidateRef("env:" + dbURLVar); err == nil {
		t.Fatalf("trailing-star prefix admitted env:%s", dbURLVar)
	}
}

// The refusal must name the rule and the knob so an admin can act on it, and
// must not name the variable: SecretsProvider's contract forbids echoing a
// locator, because the secretRef field is where a raw secret eventually gets
// pasted by mistake and this text reaches HTTP responses and logs.
func TestEnvAllowErrorNamesTheRuleNotTheVariable(t *testing.T) {
	t.Setenv(envAllowVar, "")
	err := NewResolver().ValidateRef("env:ghp_notarealtokenbutpastedanyway")
	if err == nil {
		t.Fatal("want a refusal")
	}
	msg := err.Error()
	if strings.Contains(msg, "ghp_notarealtokenbutpastedanyway") {
		t.Fatalf("refusal echoes the locator: %s", msg)
	}
	if !strings.Contains(msg, envAllowVar) {
		t.Fatalf("refusal does not name %s, so an admin cannot act on it: %s", envAllowVar, msg)
	}
	if !strings.Contains(msg, "ORBEAT_UPSTREAM_") {
		t.Fatalf("refusal does not name the allowed prefixes: %s", msg)
	}
}

// The zero value must be the RESTRICTED one. This is the property that makes
// the allowlist impossible to skip by construction: a future caller who writes
// EnvProvider{} without thinking about policy gets the default list, not
// allow-everything.
func TestZeroEnvProviderIsRestricted(t *testing.T) {
	t.Setenv(envAllowVar, "")
	if err := (EnvProvider{}).ValidateLocator(dbURLVar); err == nil {
		t.Fatalf("EnvProvider{}.ValidateLocator(%s) = nil; the zero value must enforce the default allowlist", dbURLVar)
	}
}

// NewProcessConfigResolver is the deliberate hole, and it needs a test as much
// as the allowlist does: cmd/api resolves ORBEAT_SCAN_LLM_KEY_REF through it at
// startup, and deploy/docker-compose.yml sets that to "env:ORBEAT_FAKE_LLM_KEY",
// a name the default allowlist refuses. If this ever starts failing, the
// committed dev stack stops booting.
func TestProcessConfigResolverIgnoresTheAllowlist(t *testing.T) {
	t.Setenv(envAllowVar, "")
	t.Setenv("ORBEAT_FAKE_LLM_KEY", "dev-fake-llm-key-not-a-secret")
	r := NewProcessConfigResolver()
	got, err := r.Resolve(context.Background(), "env:ORBEAT_FAKE_LLM_KEY")
	if err != nil {
		t.Fatalf("process-config resolve: %v", err)
	}
	if got != "dev-fake-llm-key-not-a-secret" {
		t.Fatalf("got %q", got)
	}
	// It is still a resolver, not a bypass of everything else.
	if err := r.ValidateRef("env:"); err == nil {
		t.Error("process-config resolver accepted an empty locator")
	}
	if err := r.ValidateRef("valut:kv/x#t"); err == nil {
		t.Error("process-config resolver accepted an unregistered scheme")
	}
}

// parseEnvAllow's contract, asserted directly so the table above cannot be the
// only thing standing between an empty prefix and every variable.
func TestParseEnvAllowNeverYieldsAnEmptyPrefix(t *testing.T) {
	for _, raw := range []string{"", " ", ",", "*", " *, ,* ", "a,,b", "  X_ , Y_* "} {
		for _, p := range parseEnvAllow(raw) {
			if p == "" {
				t.Fatalf("parseEnvAllow(%q) yielded an empty prefix, which matches every variable", raw)
			}
		}
	}
	got := parseEnvAllow("  X_ , Y_* ")
	if len(got) != 2 || got[0] != "X_" || got[1] != "Y_" {
		t.Fatalf("parseEnvAllow trimming/star-stripping = %q, want [X_ Y_]", got)
	}
}
