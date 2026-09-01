package syncclient

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Metadata is the subset of OIDC discovery the device flow needs.
type Metadata struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
	// AuthorizationEndpoint is used only by the loopback + PKCE login
	// (`orbeat-sync login --browser`). It is deliberately NOT required by
	// Discover: the device flow is the supported path everywhere, and failing
	// discovery over a field only one optional flow reads would break login
	// against a provider that simply does not advertise it.
	AuthorizationEndpoint string `json:"authorization_endpoint"`
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
	if err := decodeJSONCapped(resp.Body, maxJSONBodyBytes, &m); err != nil {
		return Metadata{}, fmt.Errorf("oidc discovery: %w", err)
	}
	if m.DeviceAuthorizationEndpoint == "" || m.TokenEndpoint == "" {
		return Metadata{}, fmt.Errorf("oidc discovery: missing device_authorization_endpoint or token_endpoint")
	}
	return m, nil
}
