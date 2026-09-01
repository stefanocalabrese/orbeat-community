package syncclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"device_authorization_endpoint": "https://kc/auth/device",
			"token_endpoint": "https://kc/token"
		}`))
	}))
	defer srv.Close()

	meta, err := Discover(context.Background(), http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if meta.DeviceAuthorizationEndpoint != "https://kc/auth/device" || meta.TokenEndpoint != "https://kc/token" {
		t.Fatalf("bad metadata: %+v", meta)
	}
}

func TestDiscoverMissingEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"token_endpoint":"https://kc/token"}`)) // no device endpoint
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), http.DefaultClient, srv.URL); err == nil {
		t.Fatal("expected error when device_authorization_endpoint is absent")
	}
}

func TestDiscoverNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), http.DefaultClient, srv.URL); err == nil {
		t.Fatal("expected error on non-200 status")
	}
}

// B26: the discovery document comes from ORBEAT_OIDC_DISCOVERY_URL, client
// config this same client does not fully control — a bare
// json.NewDecoder(resp.Body).Decode had no upper bound.
func TestDiscoverRefusesAnOversizedResponseBody(t *testing.T) {
	huge := strings.Repeat("A", int(maxJSONBodyBytes)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_authorization_endpoint":"`))
		_, _ = w.Write([]byte(huge))
		_, _ = w.Write([]byte(`","token_endpoint":"https://kc/token"}`))
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), http.DefaultClient, srv.URL); err == nil {
		t.Fatal("an oversized discovery document must be refused")
	}
}
