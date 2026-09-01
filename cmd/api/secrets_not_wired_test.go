package main

import "testing"

// TestRunDoesNotWireSecretsIntoTheServer is the INVERSE of this package's other
// wiring gates, and it exists because the two directions are not symmetric in
// consequence. TestAllServerInstallersAreWiredOrExempt and its hand-written
// siblings (TestRunWiresDCRClient, TestRunWiresContactEmail) all fail when an
// installer is NOT called, catching the inert-feature class. This one fails
// when internal/api.Server.SetSecrets IS called, catching a security
// regression, and nothing else in the tree does.
//
// The value at risk is a specific local. cmd/api's run() builds
// secrets.NewProcessConfigResolver(), whose "env" provider has the
// ORBEAT_SECRET_ENV_ALLOW allowlist OFF, because the three refs it resolves
// (ORBEAT_MARKETPLACE_GIT_CREDENTIAL_REF, ORBEAT_SCAN_LLM_KEY_REF,
// ORBEAT_DCR_CLIENT_SECRET_REF) come out of the process environment itself,
// where an allowlist prevents nothing: whoever sets them already owns that
// environment. api.New separately installs secrets.NewResolver(), the
// allowlisted CATALOG resolver, and that is the one that must serve
// admin-supplied mcp_server.secret_ref / tls_ca_ref values.
//
// Adding one line, `srv.SetSecrets(secretsResolver)`, anywhere in run() routes
// catalog refs through the unrestricted resolver and reopens audit A4 in full:
// any admin could again write secretRef: "env:ORBEAT_DB_URL" on a server
// pointed at a host they chose, and the gateway would mail the Postgres
// password out as a bearer token. Measured before this file existed: the whole
// suite stays green with that line added. TestAllServerInstallersAreWiredOrExempt
// cannot see it, because it only errors when an installer is neither called nor
// exempt, so CALLING an exempt installer passes silently; and
// TestExemptInstallersNameARealInstallerWithAReason only checks that the
// exemption still names a real method. Until now the sole protection was the
// prose comment at cmd/api/main.go:100-107, and the comment immediately above
// it argues TOWARD one shared resolver ("reusing one Resolver avoids each site
// building and authenticating its own client"), which is exactly the reasoning
// that produces the defect.
//
// Derivation, not a literal: the receiver is found by tracing run()'s
// assignment from api.New(...) (installerReceiverVar), and the call set comes
// from calledServerSetters, the same two helpers the positive gate uses. So a
// rename of the srv local cannot blind this check while leaving its sibling
// working, and both gates read one source of truth about what run() does.
//
// The non-vacuity guard is load-bearing. A negative assertion over a scan is
// worth nothing if the scan can return empty for an unrelated reason (a
// refactor that moves every installer call into a helper, a future helper bug):
// "SetSecrets was not found" would then be indistinguishable from "nothing was
// found". run() calls nine other Set* methods on srv today, so an empty result
// means the machinery broke, not that the property holds.
//
// It refuses ANY SetSecrets call, including a harmless
// `srv.SetSecrets(secrets.NewResolver())`. That strictness is deliberate: the
// exemption in installer_wiring_test.go says cmd/api does not call this
// installer at all, and a redundant call would put the catalog resolver's
// identity in a second place, where the next edit picks the wrong local. If a
// deployment binary ever legitimately needs to swap the provider set, it
// changes the exemption and this gate together, which is the review surface.
//
// Known limitation, shared with every gate in this package: it matches Set*
// calls on the identifier run() assigns from api.New. A mutant that aliases
// that local first (`s := srv; s.SetSecrets(...)`) evades it, as it evades
// TestRunWiresDCRClient in the other direction.
func TestRunDoesNotWireSecretsIntoTheServer(t *testing.T) {
	runFn := parseRunFunc(t, "main.go")

	recv, ok := installerReceiverVar(runFn, "api", "New")
	if !ok {
		t.Fatal("run() has no local assigned from api.New(...); cannot tell whether SetSecrets is " +
			"called on the *api.Server, so this gate would otherwise pass vacuously")
	}
	called := calledServerSetters(runFn, recv)
	if len(called) == 0 {
		t.Fatalf("calledServerSetters found zero Set* calls on %s in run() (there are 9 today), so "+
			"the scan is broken and the absence of SetSecrets below would prove nothing", recv)
	}

	if called["SetSecrets"] {
		t.Errorf("run() calls %s.SetSecrets, which overrides api.New's allowlisted catalog secrets "+
			"resolver. The only resolver in scope there is secretsResolver, built by "+
			"secrets.NewProcessConfigResolver() with ORBEAT_SECRET_ENV_ALLOW switched OFF, so this "+
			"routes admin-supplied mcp_server.secret_ref values through an unrestricted env provider "+
			"and reopens audit A4: secretRef \"env:ORBEAT_DB_URL\" on an attacker-chosen endpoint mails "+
			"the Postgres password out as a bearer token. The process-config resolver is for the three "+
			"refs that come from the environment itself and must never serve the catalog; see "+
			"NewProcessConfigResolver's doc comment and the SetSecrets entry in "+
			"exemptServerInstallers", recv)
	}
}
