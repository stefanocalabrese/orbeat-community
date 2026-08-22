package syncclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Artifact is one entitled artifact returned by GET /v1/sync/artifacts. Content
// is the final, ready-to-write file body (memory frontmatter already injected).
// MemoryScope/MemorySeed are set only for user/project-scope subagents that
// carry a governed seed (spec §6).
type Artifact struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Content     string `json:"content"`
	MemoryScope string `json:"memoryScope,omitempty"` // seed target-path selection (user|project)
	MemorySeed  string `json:"memorySeed,omitempty"`  // governed ORBEAT-SEED block body
}

// authHint annotates a 401/403 — the server rejected the presented token — with
// the one command that fixes it. Other statuses (5xx, 404, …) are not credential
// problems, so they get no hint: sending the user to re-login would mislead.
func authHint(status int) string {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return " (token rejected — run 'orbeat-sync login')"
	}
	return ""
}

// FetchArtifacts calls GET {baseURL}/v1/sync/artifacts with the bearer token and
// returns the caller's entitled artifacts.
func FetchArtifacts(ctx context.Context, hc *http.Client, baseURL, accessToken string) ([]Artifact, error) {
	url := strings.TrimRight(baseURL, "/") + "/v1/sync/artifacts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch artifacts: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch artifacts: status %d%s", resp.StatusCode, authHint(resp.StatusCode))
	}
	var body struct {
		Artifacts []Artifact `json:"artifacts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("fetch artifacts: decode: %w", err)
	}
	return body.Artifacts, nil
}

// FetchGatewayURL calls GET {baseURL}/v1/sync/config and returns the gateway URL
// orbeat-sync writes into each tool's MCP config.
func FetchGatewayURL(ctx context.Context, hc *http.Client, baseURL, accessToken string) (string, error) {
	url := strings.TrimRight(baseURL, "/") + "/v1/sync/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch gateway url: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch gateway url: status %d%s", resp.StatusCode, authHint(resp.StatusCode))
	}
	var body struct {
		GatewayURL string `json:"gateway_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("fetch gateway url: decode: %w", err)
	}
	if body.GatewayURL == "" {
		return "", fmt.Errorf("fetch gateway url: server returned an empty gateway_url")
	}
	return body.GatewayURL, nil
}
