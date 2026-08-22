package marketplace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateWritesValidManifests(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir, Options{GatewayURL: "http://localhost:8090"}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var mp marketplaceManifest
	readJSON(t, filepath.Join(dir, ".claude-plugin", "marketplace.json"), &mp)
	if mp.Name != "orbeat" {
		t.Errorf("marketplace name = %q, want orbeat", mp.Name)
	}
	if len(mp.Plugins) != 1 || mp.Plugins[0].Name != "orbeat-gateway" {
		t.Fatalf("plugins = %+v, want one orbeat-gateway", mp.Plugins)
	}
	if mp.Plugins[0].Source != "./plugins/orbeat-gateway" {
		t.Errorf("plugin source = %q, want ./plugins/orbeat-gateway", mp.Plugins[0].Source)
	}

	var pm pluginManifest
	readJSON(t, filepath.Join(dir, "plugins", "orbeat-gateway", ".claude-plugin", "plugin.json"), &pm)
	if pm.Name != "orbeat-gateway" || pm.Version != PluginVersion {
		t.Errorf("plugin manifest = %+v, want name orbeat-gateway version %s", pm, PluginVersion)
	}

	var mc mcpConfig
	readJSON(t, filepath.Join(dir, "plugins", "orbeat-gateway", ".mcp.json"), &mc)
	srv, ok := mc.MCPServers["orbeat-gateway"]
	if !ok {
		t.Fatalf("mcpServers missing orbeat-gateway: %+v", mc.MCPServers)
	}
	if srv.Type != "http" || srv.URL != "http://localhost:8090/mcp" {
		t.Errorf("mcp server = %+v, want http http://localhost:8090/mcp", srv)
	}
}

func TestGenerateTrimsTrailingSlashOnGatewayURL(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir, Options{GatewayURL: "https://gw.example/"}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var mc mcpConfig
	readJSON(t, filepath.Join(dir, "plugins", "orbeat-gateway", ".mcp.json"), &mc)
	if got := mc.MCPServers["orbeat-gateway"].URL; got != "https://gw.example/mcp" {
		t.Errorf("mcp url = %q, want https://gw.example/mcp", got)
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	opts := Options{GatewayURL: "http://localhost:8090"}
	if err := Generate(a, opts); err != nil {
		t.Fatal(err)
	}
	if err := Generate(b, opts); err != nil {
		t.Fatal(err)
	}
	for _, rel := range relFiles() {
		if string(mustRead(t, filepath.Join(a, rel))) != string(mustRead(t, filepath.Join(b, rel))) {
			t.Errorf("%s not deterministic", rel)
		}
	}
}

func relFiles() []string {
	return []string{
		filepath.Join(".claude-plugin", "marketplace.json"),
		filepath.Join("plugins", "orbeat-gateway", ".claude-plugin", "plugin.json"),
		filepath.Join("plugins", "orbeat-gateway", ".mcp.json"),
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := json.Unmarshal(mustRead(t, path), v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
