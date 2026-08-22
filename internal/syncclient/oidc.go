package syncclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Metadata is the subset of OIDC discovery the device flow needs.
type Metadata struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

// Discover fetches {base}/.well-known/openid-configuration and returns the device
// + token endpoints. Errors if either is missing.
func Discover(ctx context.Context, hc *http.Client, base string) (Metadata, error) {
	url := strings.TrimRight(base, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Metadata{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Metadata{}, fmt.Errorf("oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Metadata{}, fmt.Errorf("oidc discovery: status %d", resp.StatusCode)
	}
	var m Metadata
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return Metadata{}, fmt.Errorf("oidc discovery: decode: %w", err)
	}
	if m.DeviceAuthorizationEndpoint == "" || m.TokenEndpoint == "" {
		return Metadata{}, fmt.Errorf("oidc discovery: missing device_authorization_endpoint or token_endpoint")
	}
	return m, nil
}
