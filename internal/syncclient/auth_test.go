package syncclient

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mockAS returns an httptest server simulating Keycloak's device + token endpoints.
// The token endpoint replies authorization_pending once, then issues tokens.
func mockAS(t *testing.T, grantType string) (*httptest.Server, *int) {
	t.Helper()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/auth/device":
			if r.Form.Get("client_id") != "orbeat-cli" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"WXYZ-1234",
				"verification_uri":"https://kc/device","verification_uri_complete":"https://kc/device?user_code=WXYZ-1234",
				"expires_in":600,"interval":1}`))
		case "/token":
			if r.Form.Get("grant_type") != grantType || r.Form.Get("device_code") != "DC" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
				return
			}
			polls++
			if polls < 2 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"AT","refresh_token":"RT","expires_in":300}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, &polls
}

func TestLoginDeviceFlow(t *testing.T) {
	srv, polls := mockAS(t, "urn:ietf:params:oauth:grant-type:device_code")
	defer srv.Close()

	a := &Authenticator{HTTPClient: srv.Client(), ClientID: "orbeat-cli", Sleep: func(context.Context, time.Duration) error { return nil }}
	meta := Metadata{DeviceAuthorizationEndpoint: srv.URL + "/auth/device", TokenEndpoint: srv.URL + "/token"}
	var out bytes.Buffer

	tok, err := a.Login(context.Background(), meta, &out)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tok.AccessToken != "AT" || tok.RefreshToken != "RT" {
		t.Fatalf("bad token: %+v", tok)
	}
	if !tok.Expiry.After(time.Now()) {
		t.Fatalf("expiry not set in the future: %v", tok.Expiry)
	}
	if *polls < 2 {
		t.Fatalf("expected at least 2 polls (pending then success), got %d", *polls)
	}
	// The user-facing prompt must surface the verification URL + code.
	if s := out.String(); !bytes.Contains([]byte(s), []byte("WXYZ-1234")) || !bytes.Contains([]byte(s), []byte("https://kc/device")) {
		t.Fatalf("prompt missing verification details: %q", s)
	}
}

func TestLoginAccessDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path == "/auth/device" {
			_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"U","verification_uri":"https://kc/d","expires_in":600,"interval":1}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	}))
	defer srv.Close()
	a := &Authenticator{HTTPClient: srv.Client(), ClientID: "orbeat-cli", Sleep: func(context.Context, time.Duration) error { return nil }}
	meta := Metadata{DeviceAuthorizationEndpoint: srv.URL + "/auth/device", TokenEndpoint: srv.URL + "/token"}
	if _, err := a.Login(context.Background(), meta, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error on access_denied")
	}
}

func TestRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path == "/token" && r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "RT" {
			_, _ = w.Write([]byte(`{"access_token":"AT2","refresh_token":"RT2","expires_in":300}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()
	a := &Authenticator{HTTPClient: srv.Client(), ClientID: "orbeat-cli", Sleep: func(context.Context, time.Duration) error { return nil }}
	tok, err := a.Refresh(context.Background(), srv.URL+"/token", "RT")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok.AccessToken != "AT2" || tok.RefreshToken != "RT2" {
		t.Fatalf("bad refreshed token: %+v", tok)
	}
}

// B26: the device-authorization response comes from the OIDC token endpoint,
// client config this client does not fully control — a bare
// json.NewDecoder(resp.Body).Decode had no upper bound.
//
// Exercises requestDeviceCode DIRECTLY rather than through Login: an earlier
// version of this test drove it through Login and passed even on the
// unfixed code, but for the WRONG reason — Login's meta.TokenEndpoint was
// never served by the fake server, so the very next step (the token poll)
// 404'd, failed to decode a non-JSON body, and Login gave up after
// maxConsecutivePollErrors, an error that has nothing to do with the device-
// auth response's size. Calling requestDeviceCode directly removes that
// confound.
func TestRequestDeviceCodeRefusesAnOversizedResponseBody(t *testing.T) {
	huge := strings.Repeat("A", int(maxJSONBodyBytes)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"`))
		_, _ = w.Write([]byte(huge))
		_, _ = w.Write([]byte(`","user_code":"U","verification_uri":"https://kc/d","expires_in":600,"interval":1}`))
	}))
	defer srv.Close()
	a := &Authenticator{HTTPClient: srv.Client(), ClientID: "orbeat-cli"}
	if _, err := a.requestDeviceCode(context.Background(), srv.URL); err == nil {
		t.Fatal("an oversized device-authorization response must be refused")
	}
}

// Same defect on the token-endpoint poll response (requestDeviceCode's
// sibling decode), exercised through Refresh: an access/refresh token pair is
// ordinarily small, but nothing bounded it before this fix.
func TestRefreshRefusesAnOversizedTokenResponse(t *testing.T) {
	huge := strings.Repeat("A", int(maxJSONBodyBytes)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"`))
		_, _ = w.Write([]byte(huge))
		_, _ = w.Write([]byte(`","refresh_token":"RT2","expires_in":300}`))
	}))
	defer srv.Close()
	a := &Authenticator{HTTPClient: srv.Client(), ClientID: "orbeat-cli", Sleep: func(context.Context, time.Duration) error { return nil }}
	if _, err := a.Refresh(context.Background(), srv.URL+"/token", "RT"); err == nil {
		t.Fatal("an oversized token-endpoint response must be refused")
	}
}

func TestLoginSlowDownIncrementsInterval(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/auth/device":
			_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"U","verification_uri":"https://kc/d","expires_in":600,"interval":1}`))
		case "/token":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"slow_down"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"AT","refresh_token":"RT","expires_in":300}`))
		}
	}))
	defer srv.Close()
	var durs []time.Duration
	a := &Authenticator{HTTPClient: srv.Client(), ClientID: "orbeat-cli",
		Sleep: func(_ context.Context, d time.Duration) error { durs = append(durs, d); return nil }}
	meta := Metadata{DeviceAuthorizationEndpoint: srv.URL + "/auth/device", TokenEndpoint: srv.URL + "/token"}
	tok, err := a.Login(context.Background(), meta, &bytes.Buffer{})
	if err != nil || tok.AccessToken != "AT" {
		t.Fatalf("login: %v %+v", err, tok)
	}
	if len(durs) < 2 || durs[1] <= durs[0] {
		t.Fatalf("slow_down should increase the interval, got %v", durs)
	}
}

func TestLoginExpiresBeforeApproval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path == "/auth/device" {
			_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"U","verification_uri":"https://kc/d","expires_in":3,"interval":1}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`)) // never approves
	}))
	defer srv.Close()
	a := &Authenticator{HTTPClient: srv.Client(), ClientID: "orbeat-cli",
		Sleep: func(context.Context, time.Duration) error { return nil }}
	meta := Metadata{DeviceAuthorizationEndpoint: srv.URL + "/auth/device", TokenEndpoint: srv.URL + "/token"}
	if _, err := a.Login(context.Background(), meta, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error when the device code expires before approval")
	}
}

func TestRefreshFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()
	a := &Authenticator{HTTPClient: srv.Client(), ClientID: "orbeat-cli",
		Sleep: func(context.Context, time.Duration) error { return nil }}
	if _, err := a.Refresh(context.Background(), srv.URL+"/token", "RT"); err == nil {
		t.Fatal("expected error on invalid_grant refresh")
	}
}
