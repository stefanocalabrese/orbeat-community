// Package marketplace renders a Claude Code plugin marketplace that distributes
// the orbeat gateway as a single thin remote-MCP plugin. The marketplace is
// identical for every user — the gateway enforces per-identity RBAC at runtime —
// so generation is parameterized only by the gateway's public URL. Output is
// deterministic so the dev-default tree can be committed and golden-tested.
package marketplace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// MarketplaceName is the marketplace identifier (users reference @orbeat).
	MarketplaceName = "orbeat"
	// PluginName is the single plugin distributed by this marketplace.
	PluginName = "orbeat-gateway"
	// PluginSource is the plugin path relative to the marketplace root. Relative
	// sources resolve when the marketplace is added via git or a local directory
	// path (the two modes orbeat supports).
	PluginSource = "./plugins/" + PluginName
	// PluginVersion is stamped into the plugin manifest.
	PluginVersion = "0.1.0"

	marketplaceDescription = "orbeat - governed AI-agent capabilities"
	pluginDescription      = "Connect to your entitled orbeat MCP gateway (SSO/OAuth)."
	ownerName              = "orbeat"
)

// Options parameterize generation.
type Options struct {
	// GatewayURL is the gateway public base URL (no trailing slash, no /mcp),
	// e.g. http://localhost:8090. "/mcp" is appended for the MCP server URL.
	GatewayURL string
}

type ownerRef struct {
	Name string `json:"name"`
}

type pluginEntry struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Description string `json:"description"`
}

type marketplaceManifest struct {
	Name        string        `json:"name"`
	Owner       ownerRef      `json:"owner"`
	Description string        `json:"description"`
	Plugins     []pluginEntry `json:"plugins"`
}

type pluginManifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      ownerRef `json:"author"`
}

type mcpServer struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type mcpConfig struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

// MCPURL returns the gateway MCP endpoint for the given base URL.
func MCPURL(gatewayURL string) string {
	return strings.TrimSuffix(gatewayURL, "/") + "/mcp"
}

// mustJSON serialises v to indented JSON with a trailing newline. Panics only
// on unencodable types (should never happen with our well-typed structs).
func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("marketplace: mustJSON: %v", err))
	}
	return string(b) + "\n"
}

// renderConnectPlugin returns the orbeat-gateway plugin files as a map of
// relative path → content. The paths and content are identical to what
// Generate writes.
func renderConnectPlugin(opts Options) map[string]string {
	pm := pluginManifest{
		Name:        PluginName,
		Version:     PluginVersion,
		Description: pluginDescription,
		Author:      ownerRef{Name: ownerName},
	}
	mc := mcpConfig{MCPServers: map[string]mcpServer{
		PluginName: {Type: "http", URL: MCPURL(opts.GatewayURL)},
	}}
	base := "plugins/" + PluginName
	return map[string]string{
		base + "/.claude-plugin/plugin.json": mustJSON(pm),
		base + "/.mcp.json":                  mustJSON(mc),
	}
}

// renderMarketplaceManifest returns the .claude-plugin/marketplace.json content.
// When withArtifacts is true, the orbeat-artifacts plugin entry is appended.
func renderMarketplaceManifest(withArtifacts bool) string {
	plugins := []pluginEntry{{
		Name:        PluginName,
		Source:      PluginSource,
		Description: pluginDescription,
	}}
	if withArtifacts {
		plugins = append(plugins, pluginEntry{
			Name:        ArtifactsPluginName,
			Source:      "./plugins/" + ArtifactsPluginName,
			Description: "orbeat governed skills and subagents.",
		})
	}
	mp := marketplaceManifest{
		Name:        MarketplaceName,
		Owner:       ownerRef{Name: ownerName},
		Description: marketplaceDescription,
		Plugins:     plugins,
	}
	return mustJSON(mp)
}

// Generate writes the marketplace tree rooted at dir, creating all required
// subdirectories.
func Generate(dir string, opts Options) error {
	if opts.GatewayURL == "" {
		return fmt.Errorf("marketplace: GatewayURL is required")
	}

	files := map[string]string{
		filepath.Join(".claude-plugin", "marketplace.json"): renderMarketplaceManifest(false),
	}
	for rel, content := range renderConnectPlugin(opts) {
		files[rel] = content
	}

	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := writeJSON(path, content); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(path string, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("marketplace: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("marketplace: write %s: %w", path, err)
	}
	return nil
}
