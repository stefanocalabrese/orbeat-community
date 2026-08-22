package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// These tests exercise the real run() boot path (not a private helper) up to
// and past the marketplace-git-credential-ref startup check. cfg.Load and
// cfg.RequireOIDC do no I/O, so setting the three vars they require plus
// ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF gets run() to the check without a
// live Keycloak/Postgres — ORBEAT_DB_URL points at 127.0.0.1:1, a port
// nothing listens on, so any DB dial fails with an instant "connection
// refused" rather than a multi-second timeout.
//
// internal/secrets/validate_test.go already covers every ValidateRef
// input/output pairing in isolation. What that does NOT cover is the wiring:
// that run() actually calls the check, at the right point, with the right
// config field. That is what these two tests assert, by distinguishing WHICH
// fatal fired — "validate ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF" vs.
// "migrate" — rather than merely "run() returned non-zero" (a weaker
// assertion that a check anywhere in the path, or none at all given the
// unreachable DB, would also satisfy).

// TestRunAbortsOnMalformedMarketplaceGitCredentialRef: a malformed ref must
// abort at THIS check, before the DB is ever dialed. If this check stopped
// firing, run() would instead fail at "migrate" against the unreachable
// ORBEAT_DB_URL — this test would then fail on the "migrate" assertion below
// instead of passing vacuously on the (still non-zero) exit code alone.
func TestRunAbortsOnMalformedMarketplaceGitCredentialRef(t *testing.T) {
	setBootEnv(t, "valut:kv/x#t")

	out, code := runCapturingStdout(t)

	if code == 0 {
		t.Fatalf("run() = 0, want non-zero (a malformed ref must abort boot)")
	}
	if !strings.Contains(out, "validate ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF") {
		t.Errorf("run() output does not name the credential-ref check; got:\n%s", out)
	}
	if strings.Contains(out, `"msg":"migrate"`) {
		t.Errorf("run() reached the migrate step — the credential-ref check did not abort boot before it; got:\n%s", out)
	}
	// Same no-echo discipline as the rest of this validation slice: the
	// malformed ref itself must never appear in the boot-abort log line.
	if strings.Contains(out, "valut:kv/x#t") {
		t.Errorf("run() output echoes the malformed ref: %s", out)
	}
}

// TestRunProceedsPastWellFormedRefToMigrate: a well-formed ref (even to a
// backend orbeat-api never contacts here) must NOT abort at this check. It
// should sail through and fail later at "migrate" against the deliberately
// unreachable ORBEAT_DB_URL — proving the call site performs no I/O
// (internal/secrets/validate_test.go's TestValidateRefDoesNotResolve already
// proves this for ValidateRef in isolation; this proves the boot wiring
// doesn't smuggle in a resolve some other way). Uses env:, not vault: — the
// scheme is incidental to what this test checks, and env: is the one scheme
// registered in every tier (docs/specs/2026-08-19-orbeat-community-repo-
// generation-design.md §4).
func TestRunProceedsPastWellFormedRefToMigrate(t *testing.T) {
	setBootEnv(t, "env:ORBEAT_TEST_GIT_TOKEN")

	out, code := runCapturingStdout(t)

	if code == 0 {
		t.Fatalf("run() = 0, want non-zero (ORBEAT_DB_URL is unreachable)")
	}
	if strings.Contains(out, "validate ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF") {
		t.Errorf("run() aborted at the credential-ref check on a WELL-FORMED ref; got:\n%s", out)
	}
	if !strings.Contains(out, `"msg":"migrate"`) {
		t.Errorf("run() did not reach the migrate step; got:\n%s", out)
	}
}

// setBootEnv sets the three vars config.Load/RequireOIDC need (none require
// I/O to validate) plus the marketplace git credential ref under test.
// ORBEAT_DB_URL deliberately targets a port nothing listens on.
func setBootEnv(t *testing.T, credentialRef string) {
	t.Helper()
	t.Setenv("ORBEAT_DB_URL", "postgres://orbeat:orbeat@127.0.0.1:1/orbeat?sslmode=disable")
	t.Setenv("ORBEAT_OIDC_ISSUER", "http://localhost:8088/realms/orbeat")
	t.Setenv("ORBEAT_OIDC_AUDIENCE", "orbeat-api")
	t.Setenv("ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF", credentialRef)
}

// runCapturingStdout redirects the process-wide os.Stdout for the duration of
// run() — the only way to observe its slog output, since logging.New(os.Stdout,
// ...) is hard-coded inside run() — and restores it afterward.
func runCapturingStdout(t *testing.T) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	code := run()
	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String(), code
}
