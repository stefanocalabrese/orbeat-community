package syncclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// Authenticator runs the OAuth 2.0 Device Authorization Grant against Keycloak.
// Sleep is injectable so tests don't wait real intervals.
type Authenticator struct {
	HTTPClient *http.Client
	ClientID   string
	Sleep      func(context.Context, time.Duration) error
}

type deviceAuthResp struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type tokenResp struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// maxConsecutivePollErrors is how many back-to-back transport failures the
// device-flow poll loop tolerates before giving up. A single transient error (a
// dropped connection, a momentary DNS blip) should not abort a login the user is
// midway through approving; the device code's own expiry still bounds the loop,
// so this can never spin forever.
const maxConsecutivePollErrors = 3

// tokenErrMsg builds a non-empty, diagnosable message from a token-endpoint
// failure: the OAuth error code, its error_description when present, and the
// HTTP status as a fallback so the message is never blank (a bare
// "device login failed: " told the user nothing).
func tokenErrMsg(tr tokenResp, status int) string {
	switch {
	case tr.Error != "" && tr.ErrorDescription != "":
		return tr.Error + ": " + tr.ErrorDescription
	case tr.Error != "":
		return tr.Error
	case tr.ErrorDescription != "":
		return tr.ErrorDescription
	default:
		return fmt.Sprintf("unexpected status %d", status)
	}
}

// Login performs the full device flow: request a device code, print the
// verification prompt to out, then poll the token endpoint until the user
// approves (or the request is denied/expires). Returns the issued Token.
func (a *Authenticator) Login(ctx context.Context, meta Metadata, out io.Writer) (Token, error) {
	da, err := a.requestDeviceCode(ctx, meta.DeviceAuthorizationEndpoint)
	if err != nil {
		return Token{}, err
	}
	uri := da.VerificationURIComplete
	if uri == "" {
		uri = da.VerificationURI
	}
	fmt.Fprintf(out, "To sign in, visit:\n    %s\nand enter code:  %s\nWaiting for approval...\n", uri, da.UserCode)

	interval := da.Interval
	if interval <= 0 {
		interval = 5
	}
	var elapsed time.Duration
	consecutiveErrs := 0
	for {
		wait := time.Duration(interval) * time.Second
		if err := a.Sleep(ctx, wait); err != nil {
			return Token{}, err
		}
		elapsed += wait
		if da.ExpiresIn > 0 && elapsed > time.Duration(da.ExpiresIn)*time.Second {
			return Token{}, fmt.Errorf("device login: code expired before approval")
		}
		tr, status, err := a.poll(ctx, meta.TokenEndpoint, url.Values{
			"grant_type":  {deviceGrantType},
			"device_code": {da.DeviceCode},
			"client_id":   {a.ClientID},
		})
		if err != nil {
			// A transient transport error must not abort a login the user is midway
			// through approving. Tolerate a few in a row; the device-code expiry
			// above still bounds the loop.
			consecutiveErrs++
			if consecutiveErrs >= maxConsecutivePollErrors {
				return Token{}, fmt.Errorf("device login: %d consecutive polling errors: %w", consecutiveErrs, err)
			}
			continue
		}
		consecutiveErrs = 0
		if status == http.StatusOK && tr.Error == "" {
			return tokenFrom(tr), nil
		}
		switch tr.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5
			continue
		default:
			return Token{}, fmt.Errorf("device login failed: %s", tokenErrMsg(tr, status))
		}
	}
}

// Refresh exchanges a refresh token for a fresh Token.
func (a *Authenticator) Refresh(ctx context.Context, tokenEndpoint, refreshToken string) (Token, error) {
	tr, status, err := a.poll(ctx, tokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {a.ClientID},
	})
	if err != nil {
		return Token{}, err
	}
	if status != http.StatusOK || tr.Error != "" {
		return Token{}, fmt.Errorf("token refresh failed: %s", tokenErrMsg(tr, status))
	}
	return tokenFrom(tr), nil
}

func (a *Authenticator) requestDeviceCode(ctx context.Context, endpoint string) (deviceAuthResp, error) {
	form := url.Values{"client_id": {a.ClientID}, "scope": {"openid offline_access"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return deviceAuthResp{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return deviceAuthResp{}, fmt.Errorf("device authorization: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return deviceAuthResp{}, fmt.Errorf("device authorization: status %d", resp.StatusCode)
	}
	var da deviceAuthResp
	if err := decodeJSONCapped(resp.Body, maxJSONBodyBytes, &da); err != nil {
		return deviceAuthResp{}, fmt.Errorf("device authorization: %w", err)
	}
	return da, nil
}

func (a *Authenticator) poll(ctx context.Context, endpoint string, form url.Values) (tokenResp, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResp{}, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return tokenResp{}, 0, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	var tr tokenResp
	if err := decodeJSONCapped(resp.Body, maxJSONBodyBytes, &tr); err != nil {
		return tokenResp{}, resp.StatusCode, fmt.Errorf("token endpoint: %w", err)
	}
	return tr, resp.StatusCode, nil
}

func tokenFrom(tr tokenResp) Token {
	return Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}
}

// CtxSleep waits for d or until ctx is cancelled. It is the default
// Authenticator.Sleep used by the CLI (tests inject a no-op).
func CtxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
