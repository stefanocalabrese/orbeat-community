package syncclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Loopback + PKCE login, the opt-in alternative to the device flow behind
// `orbeat-sync login --browser`.
//
// The device flow remains the DEFAULT and the supported path everywhere: it
// needs no listener, no redirect registration, and works over SSH and in
// containers where no browser exists. This flow exists because on a desktop the
// device flow asks a person to retype a code that the machine could have
// carried itself.
//
// Three properties carry the security of this flow, and each has its own test:
//
//   - PKCE S256 (RFC 7636). The verifier never leaves this process except as a
//     SHA-256 hash in the authorization request, so an attacker who intercepts
//     the redirect (another local process racing the listener) still cannot
//     exchange the code.
//   - A `state` compared on return (RFC 6749 §10.12). A callback whose state
//     does not match is refused and the login fails rather than proceeding with
//     an authorization code this process never asked for.
//   - A LOOPBACK-ONLY listener on 127.0.0.1 with an ephemeral port (RFC 8252
//     §7.3). Binding 0.0.0.0 would let anything on the network deliver a
//     callback; binding a fixed port would collide with a second login.
const (
	// browserLoginTimeout bounds the whole flow. A person who opened the
	// browser and walked away should get their terminal back, and the listener
	// must not outlive the attempt: an abandoned one is a process holding a
	// port open waiting for anyone to talk to it.
	browserLoginTimeout = 5 * time.Minute
	// callbackPath is the single path the listener answers on. Anything else
	// gets a 404, so a stray request cannot be mistaken for the callback.
	callbackPath = "/orbeat/callback"
)

// Opener launches the user's browser at rawURL. Injected so tests drive the
// flow without a browser, and so a failure to open is a warning rather than a
// dead end: the URL is printed and a person can paste it.
type Opener func(rawURL string) error

// callbackResult is what the one-shot HTTP handler observes.
type callbackResult struct {
	code  string
	state string
	err   error
}

// LoginBrowser runs the authorization-code flow with PKCE against meta's
// authorization endpoint, capturing the redirect on a loopback listener.
func (a *Authenticator) LoginBrowser(ctx context.Context, meta Metadata, open Opener, out io.Writer) (Token, error) {
	if meta.AuthorizationEndpoint == "" {
		return Token{}, errors.New("browser login: the provider does not advertise an authorization_endpoint; use the device flow")
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		return Token{}, err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return Token{}, err
	}

	// Port 0: the OS picks a free ephemeral port, which is what RFC 8252 asks
	// for and what stops two concurrent logins from colliding.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Token{}, fmt.Errorf("browser login: listen on loopback: %w", err)
	}
	defer ln.Close()
	redirectURI := "http://" + ln.Addr().String() + callbackPath

	results := make(chan callbackResult, 1)
	srv := &http.Server{Handler: callbackHandler(results), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	authURL := authorizeURL(meta.AuthorizationEndpoint, a.ClientID, redirectURI, state, challenge)
	if open != nil {
		if oerr := open(authURL); oerr != nil {
			fmt.Fprintf(out, "Could not open a browser (%v).\nOpen this URL to sign in:\n\n  %s\n\n", oerr, authURL)
		}
	} else {
		fmt.Fprintf(out, "Open this URL to sign in:\n\n  %s\n\n", authURL)
	}

	ctx, cancel := context.WithTimeout(ctx, browserLoginTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return Token{}, fmt.Errorf("browser login: timed out after %s waiting for the callback", browserLoginTimeout)
	case res := <-results:
		if res.err != nil {
			return Token{}, fmt.Errorf("browser login: %w", res.err)
		}
		// Compared before the code is used for anything at all: a mismatched
		// state means this callback belongs to some other request, and an
		// authorization code from a request we did not make is exactly what
		// RFC 6749 §10.12 says not to redeem.
		if res.state != state {
			return Token{}, errors.New("browser login: callback state did not match; refusing the authorization code")
		}
		return a.exchangeCode(ctx, meta.TokenEndpoint, res.code, verifier, redirectURI)
	}
}

// callbackHandler answers exactly one callback and reports it. Anything on
// another path is a 404 rather than a result, so a favicon request or a probe
// cannot resolve the login.
func callbackHandler(results chan<- callbackResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != callbackPath {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			msg := e
			if d := q.Get("error_description"); d != "" {
				msg += ": " + d
			}
			writeCallbackPage(w, "Sign-in failed. You can close this tab and return to the terminal.")
			select {
			case results <- callbackResult{err: errors.New(msg)}:
			default:
			}
			return
		}
		code := q.Get("code")
		if code == "" {
			writeCallbackPage(w, "Sign-in failed: no authorization code. Close this tab and return to the terminal.")
			select {
			case results <- callbackResult{err: errors.New("callback carried no authorization code")}:
			default:
			}
			return
		}
		writeCallbackPage(w, "Signed in. You can close this tab and return to the terminal.")
		select {
		case results <- callbackResult{code: code, state: q.Get("state")}:
		default:
		}
	})
}

func writeCallbackPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// The authorization code is in this page's URL, so nothing should keep it:
	// no cache, and no referrer to carry it to whatever the user opens next.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	fmt.Fprintln(w, msg)
}

// authorizeURL builds the authorization request.
func authorizeURL(endpoint, clientID, redirectURI, state, challenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("scope", "openid offline_access")
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + q.Encode()
}

// exchangeCode redeems the authorization code with the verifier.
func (a *Authenticator) exchangeCode(ctx context.Context, tokenEndpoint, code, verifier, redirectURI string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", a.ClientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("browser login: token exchange: %w", err)
	}
	defer resp.Body.Close()
	var tr tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		// Static error text, never the body: it is a token response.
		return Token{}, errors.New("browser login: token endpoint returned an unreadable response")
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		return Token{}, fmt.Errorf("browser login failed: %s", tokenErrMsg(tr, resp.StatusCode))
	}
	return tokenFrom(tr), nil
}

// newPKCE returns a fresh verifier and its S256 challenge (RFC 7636 §4.1-4.2).
func newPKCE() (verifier, challenge string, err error) {
	verifier, err = randomURLSafe(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// randomURLSafe returns n bytes of crypto/rand as unpadded base64url.
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("browser login: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
