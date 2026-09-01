package syncclient

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/testkc"
)

// TestLoginBrowserAgainstRealKeycloak is the gate that matters for this flow,
// and it exists because the alternative was a runbook.
//
// Everything unit tests can prove here (PKCE derivation, state comparison,
// callback parsing) is proved without a server. What they CANNOT prove is the
// one thing most likely to be wrong: whether the redirect URI this client
// builds at an EPHEMERAL PORT is accepted by the realm's registration. RFC 8252
// says the loopback port is ephemeral; whether a given authorization server
// matches `http://127.0.0.1:*/orbeat/callback` against port 54231 is that
// server's business, and guessing it right is not something a mock can check.
// A mock authorization server would have validated my own assumption against
// itself.
//
// So this drives the real Keycloak with the committed dev realm, with an
// "opener" standing in for the browser: fetch the authorization page, post the
// login form, and let the redirect chain deliver the callback to the listener
// LoginBrowser opened.
func TestLoginBrowserAgainstRealKeycloak(t *testing.T) {
	ctx := context.Background()
	issuer, _ := testkc.StartKeycloak(t, ctx)

	meta, err := Discover(ctx, http.DefaultClient, issuer)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if meta.AuthorizationEndpoint == "" {
		t.Fatal("the realm advertises no authorization_endpoint, so --browser can never work against it")
	}

	a := &Authenticator{HTTPClient: &http.Client{Timeout: 30 * time.Second}, ClientID: "orbeat-cli"}
	tok, err := a.LoginBrowser(ctx, meta, browserStub(t, "alice", "alice"), io.Discard)
	if err != nil {
		t.Fatalf("browser login against real Keycloak: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatal("browser login returned an empty access token")
	}
	if tok.RefreshToken == "" {
		t.Fatal("browser login returned no refresh token, so the session cannot outlive the access token")
	}
}

// TestLoginBrowserRejectsAForeignState pins the state check against the real
// server too, because the check is worthless if the server never round-trips
// the parameter. The stub tampers with `state` on the way back, exactly as an
// attacker delivering a callback for a request this process never made would.
func TestLoginBrowserRejectsAForeignState(t *testing.T) {
	ctx := context.Background()
	issuer, _ := testkc.StartKeycloak(t, ctx)
	meta, err := Discover(ctx, http.DefaultClient, issuer)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	a := &Authenticator{HTTPClient: &http.Client{Timeout: 30 * time.Second}, ClientID: "orbeat-cli"}
	tamper := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		redirect := u.Query().Get("redirect_uri")
		// Deliver a callback carrying a code-shaped value and the WRONG state.
		resp, err := http.Get(redirect + "?code=stolen-code&state=not-the-state-we-sent")
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	if _, err := a.LoginBrowser(ctx, meta, tamper, io.Discard); err == nil {
		t.Fatal("a callback with a foreign state was accepted; the authorization code must be refused")
	} else if !strings.Contains(err.Error(), "state did not match") {
		t.Fatalf("wrong rejection reason: %v", err)
	}
}

var formActionRe = regexp.MustCompile(`(?is)<form[^>]+id="kc-form-login"[^>]*action="([^"]+)"`)

// browserStub is a headless stand-in for the user's browser: it follows the
// authorization URL, submits Keycloak's login form, and lets the resulting
// redirect chain reach the loopback listener.
func browserStub(t *testing.T, user, pass string) Opener {
	t.Helper()
	return func(raw string) error {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return err
		}
		c := &http.Client{Jar: jar, Timeout: 30 * time.Second}
		resp, err := c.Get(raw)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		m := formActionRe.FindSubmatch(body)
		if m == nil {
			t.Logf("login page did not carry the expected form:\n%s", truncate(string(body), 1200))
			return errNoLoginForm
		}
		action := strings.ReplaceAll(string(m[1]), "&amp;", "&")
		resp2, err := c.PostForm(action, url.Values{"username": {user}, "password": {pass}})
		if err != nil {
			return err
		}
		defer resp2.Body.Close()
		_, _ = io.Copy(io.Discard, resp2.Body)
		return nil
	}
}

var errNoLoginForm = errStr("keycloak login form not found")

type errStr string

func (e errStr) Error() string { return string(e) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
