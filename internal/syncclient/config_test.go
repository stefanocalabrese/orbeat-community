package syncclient

import "testing"

// TestLoadConfigDefaults pins every default LoadConfig applies. The file had
// ZERO executed statements before this test, measured with -coverprofile over
// the whole tree, in a package that otherwise carries 231 tests. Nothing here
// is clever; the point is that a wrong default is invisible without it, and
// these four values decide which server the CLI talks to and which OAuth
// client it presents.
func TestLoadConfigDefaults(t *testing.T) {
	for _, k := range []string{
		"ORBEAT_API_URL", "ORBEAT_OIDC_ISSUER",
		"ORBEAT_OIDC_DISCOVERY_URL", "ORBEAT_SYNC_CLIENT_ID",
	} {
		t.Setenv(k, "")
	}

	got := LoadConfig()
	want := Config{
		APIBaseURL: "http://localhost:8080",
		OIDCIssuer: "http://localhost:8088/realms/orbeat",
		// Not a copy of OIDCIssuer by coincidence: with the discovery URL
		// unset, LoadConfig falls back to the issuer. That fallback is the
		// reason ORBEAT_OIDC_DISCOVERY_URL can be left unset in every
		// pure-localhost deployment, so it is asserted rather than assumed.
		OIDCDiscovery: "http://localhost:8088/realms/orbeat",
		ClientID:      "orbeat-cli",
	}
	if got != want {
		t.Errorf("LoadConfig() with no environment =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestLoadConfigReadsEveryEnvVar pins that each variable is read AND that it
// lands in the field it names. A test setting one variable at a time would
// pass on an implementation that read the right variables into the wrong
// fields, so all four are set to distinct values at once.
func TestLoadConfigReadsEveryEnvVar(t *testing.T) {
	t.Setenv("ORBEAT_API_URL", "https://api.example.test")
	t.Setenv("ORBEAT_OIDC_ISSUER", "https://issuer.example.test/realms/x")
	t.Setenv("ORBEAT_OIDC_DISCOVERY_URL", "https://discovery.example.test/realms/x")
	t.Setenv("ORBEAT_SYNC_CLIENT_ID", "custom-client")

	got := LoadConfig()
	want := Config{
		APIBaseURL:    "https://api.example.test",
		OIDCIssuer:    "https://issuer.example.test/realms/x",
		OIDCDiscovery: "https://discovery.example.test/realms/x",
		ClientID:      "custom-client",
	}
	if got != want {
		t.Errorf("LoadConfig() =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestLoadConfigDiscoveryFallsBackToIssuer pins the one branch in the file:
// an explicit discovery URL wins, an unset one takes the issuer. The issuer is
// deliberately NOT the default here, so a fallback that returned the default
// issuer rather than the CONFIGURED one would fail.
func TestLoadConfigDiscoveryFallsBackToIssuer(t *testing.T) {
	t.Setenv("ORBEAT_OIDC_ISSUER", "https://configured.example.test/realms/x")
	t.Setenv("ORBEAT_OIDC_DISCOVERY_URL", "")

	if got := LoadConfig().OIDCDiscovery; got != "https://configured.example.test/realms/x" {
		t.Errorf("unset discovery URL = %q, want the configured issuer", got)
	}

	t.Setenv("ORBEAT_OIDC_DISCOVERY_URL", "https://explicit.example.test/realms/x")
	if got := LoadConfig().OIDCDiscovery; got != "https://explicit.example.test/realms/x" {
		t.Errorf("explicit discovery URL = %q, want it to win over the issuer", got)
	}
}

// TestGetenvTreatsEmptyAsUnset pins getenv's contract, which is NOT
// os.Getenv's: an empty value takes the default rather than being honoured.
// That matters because a shell exporting ORBEAT_API_URL= (a common way to
// "clear" a variable) must reach the default, not an empty base URL that
// would build requests against a relative path.
func TestGetenvTreatsEmptyAsUnset(t *testing.T) {
	t.Setenv("ORBEAT_TEST_ONLY_EMPTY", "")
	if got := getenv("ORBEAT_TEST_ONLY_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("getenv with an empty value = %q, want the default", got)
	}
	t.Setenv("ORBEAT_TEST_ONLY_EMPTY", "set")
	if got := getenv("ORBEAT_TEST_ONLY_EMPTY", "fallback"); got != "set" {
		t.Errorf("getenv with a set value = %q, want the value", got)
	}
}
