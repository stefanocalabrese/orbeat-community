// Package testkc provides a shared testcontainers-Keycloak harness for
// integration tests that need a real OIDC provider seeded with the orbeat realm.
//
// It is a regular (non-_test) package so multiple test packages can import it —
// Go cannot share helpers across packages via _test.go files. Nothing in a
// production build imports it, so testcontainers/testing stay out of the binary.
//
// Readiness gate: the container WaitingFor only confirms the HTTP port is open;
// StartKeycloak then actively polls the OIDC discovery endpoint until it reports
// the expected issuer, because Keycloak keeps warming up the realm after the
// "Listening on" log line appears.
package testkc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// StartKeycloak boots a Keycloak container with the seeded orbeat realm and waits
// until it is ready. It returns the OIDC issuer base URL and the token endpoint
// URL. The container is terminated via t.Cleanup.
//
// Callers scope this to a test function (via t.Cleanup) rather than a package
// TestMain, so it composes with a package that already has a TestMain (e.g. the
// gateway's Postgres one — Go permits only one TestMain per package).
func StartKeycloak(t testing.TB, ctx context.Context) (issuer, tokenEndpoint string) {
	t.Helper()

	realmFile := realmFilePath(t)

	kc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			// A readable, per-caller container name (instead of Docker's random
			// name) so `docker ps` shows which test owns it. Scoped by test name
			// to stay unique when several packages run their KC tests in parallel,
			// PLUS a random per-run suffix: a crashed prior run (testcontainers'
			// Ryuk reaper not yet cleaning up, or a killed test process) can leave
			// a container behind under the deterministic name, and Docker refuses
			// to create a second container with the same name — the suffix means
			// a stale leftover from a crashed run can never collide with this run.
			Name:  "orbeat-keycloak-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + randomSuffix(t),
			Image: "quay.io/keycloak/keycloak:26.7",
			Cmd:   []string{"start-dev", "--import-realm"},
			Env: map[string]string{
				"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
				"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
			},
			ExposedPorts: []string{"8080/tcp"},
			Files: []testcontainers.ContainerFile{
				{
					HostFilePath:      realmFile,
					ContainerFilePath: "/opt/keycloak/data/import/orbeat-realm.json",
					FileMode:          0o644,
				},
			},
			// "Listening on: http://0.0.0.0:8080" is emitted by KC 26 after realm
			// import completes and the main HTTP listener is bound. The active
			// discovery poll below is the real readiness gate; this log line just
			// confirms the port is open before we start polling.
			WaitingFor: wait.ForLog("Listening on: http://0.0.0.0:8080").
				WithStartupTimeout(120 * time.Second).
				WithPollInterval(2 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start keycloak container: %v", err)
	}
	t.Cleanup(func() {
		// A dedicated ctx, NOT the caller's: t.Cleanup runs after the test
		// function returns, by which point a per-test ctx the caller derived
		// (e.g. via context.WithTimeout, or a parent canceled at test end) may
		// already be Done — Terminate would then fail immediately and leak the
		// container. Bounded independently so teardown still has a chance to run.
		termCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := kc.Terminate(termCtx); err != nil {
			t.Logf("terminate keycloak: %v", err)
		}
	})

	host, err := kc.Host(ctx)
	if err != nil {
		t.Fatalf("keycloak host: %v", err)
	}
	mappedPort, err := kc.MappedPort(ctx, "8080/tcp")
	if err != nil {
		t.Fatalf("keycloak mapped port: %v", err)
	}

	base := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())
	issuer = base + "/realms/orbeat"
	tokenEndpoint = base + "/realms/orbeat/protocol/openid-connect/token"

	waitForDiscovery(t, issuer, 60*time.Second)

	return issuer, tokenEndpoint
}

// discoveryClient is a dedicated HTTP client with a per-request timeout so that
// a wedged container cannot hang a polling iteration past the intended bound.
var discoveryClient = &http.Client{Timeout: 5 * time.Second}

// waitForDiscovery polls the OIDC discovery endpoint until it returns the
// expected issuer claim or the timeout expires.
func waitForDiscovery(t testing.TB, issuer string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	discoveryURL := issuer + "/.well-known/openid-configuration"

	for time.Now().Before(deadline) {
		if discoveryReady(discoveryURL, issuer) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("keycloak discovery endpoint did not become ready at %s within %s", discoveryURL, timeout)
}

// discoveryReady performs one discovery-endpoint poll attempt, closing the
// response body exactly once (a single deferred close covers every return
// path — the previous version could close resp.Body twice on the "200 but
// issuer mismatch" path: once inside the success branch, once more in the
// unconditional cleanup below it).
func discoveryReady(discoveryURL, issuer string) bool {
	resp, err := discoveryClient.Get(discoveryURL) //nolint:noctx // polling helper, per-request timeout via client
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	return json.Unmarshal(body, &doc) == nil && doc.Issuer == issuer
}

// randomSuffix returns a short random hex string for disambiguating
// container names across runs (crypto/rand, per repo convention — see
// internal/logging.newRequestID — never math/rand, even for a non-security
// use, to keep one discipline everywhere).
func randomSuffix(t testing.TB) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// grantClient is a dedicated HTTP client with a per-request timeout so that a
// wedged container cannot hang the password-grant call past the intended bound.
var grantClient = &http.Client{Timeout: 5 * time.Second}

// PasswordGrant requests an access token via the OAuth 2.0 Resource Owner
// Password Credentials grant. Uses stdlib net/http + net/url only.
func PasswordGrant(t testing.TB, tokenEndpoint, clientID, username, password string) string {
	t.Helper()

	form := url.Values{
		"grant_type": {"password"},
		"client_id":  {clientID},
		"username":   {username},
		"password":   {password},
	}

	resp, err := grantClient.Post( //nolint:noctx // test helper, per-request timeout via client
		tokenEndpoint,
		"application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		t.Fatalf("password grant POST: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read token response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("password grant status %d: %s", resp.StatusCode, body)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if tokenResp.AccessToken == "" {
		t.Fatalf("empty access_token in response: %s", body)
	}
	return tokenResp.AccessToken
}

// realmFilePath resolves deploy/keycloak/orbeat-realm.json relative to this
// source file, walking up to the repo root. Robust regardless of the working
// directory when go test runs. internal/testkc is two directories under the
// repo root, same depth as internal/auth and internal/gateway.
func realmFilePath(t testing.TB) string {
	t.Helper()
	// runtime.Caller(0) gives the absolute path of this source file.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile: .../internal/testkc/testkc.go — repo root is two directories up.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Clean(filepath.Join(repoRoot, "deploy", "keycloak", "orbeat-realm.json"))
}
