package marketplace

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// TestGenerateIsDeterministic derives the set of files to compare from what
// Generate actually wrote into a (filesUnder), rather than a hand-maintained
// list of relative paths: a fixed list only ever proves determinism for the
// files someone remembered to name in it, and silently stops proving
// anything about a file added to (or removed from) Generate's output later.
//
// It checks BOTH directions between the two independently generated trees:
// that a and b list the exact same files (a file Generate writes only
// sometimes -- e.g. conditionally, on some future flag -- is itself a
// determinism bug this loop-only-over-a's-files would miss), and that every
// shared file's content matches byte-for-byte.
func TestGenerateIsDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	opts := Options{GatewayURL: "http://localhost:8090"}
	if err := Generate(a, opts); err != nil {
		t.Fatal(err)
	}
	if err := Generate(b, opts); err != nil {
		t.Fatal(err)
	}

	filesA := filesUnder(t, a)
	filesB := filesUnder(t, b)
	if len(filesA) == 0 {
		t.Fatal("Generate wrote zero files into a; every determinism assertion below would " +
			"compare an empty set against itself and pass over nothing")
	}
	if diff := diffFileSets(filesA, filesB); diff != "" {
		t.Fatalf("the two independently generated trees list DIFFERENT files:\n%s", diff)
	}

	for _, rel := range filesA {
		if string(mustRead(t, filepath.Join(a, rel))) != string(mustRead(t, filepath.Join(b, rel))) {
			t.Errorf("%s not deterministic", rel)
		}
	}
}

// filesUnder returns every regular file under dir, as paths relative to dir,
// sorted. Deriving the file set this way -- by asking the filesystem what is
// actually there -- is what lets a caller compare "what Generate rendered"
// against "what is committed" without either side needing a name in advance:
// a fourth file Generate starts writing, or a fifth someone deletes,
// changes this function's return value with zero code change here.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var rels []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(rels)
	return rels
}

// diffFileSets returns a human-readable description of how the sorted file
// lists a and b differ (entries only in a, entries only in b), or "" if they
// are identical. Used to compare two independently derived file sets rather
// than trusting either one to already be authoritative.
func diffFileSets(a, b []string) string {
	inA := map[string]bool{}
	for _, f := range a {
		inA[f] = true
	}
	inB := map[string]bool{}
	for _, f := range b {
		inB[f] = true
	}

	var onlyInA, onlyInB []string
	for _, f := range a {
		if !inB[f] {
			onlyInA = append(onlyInA, f)
		}
	}
	for _, f := range b {
		if !inA[f] {
			onlyInB = append(onlyInB, f)
		}
	}
	if len(onlyInA) == 0 && len(onlyInB) == 0 {
		return ""
	}
	msg := ""
	if len(onlyInA) > 0 {
		msg += "only in the first set: " + strings.Join(onlyInA, ", ") + "\n"
	}
	if len(onlyInB) > 0 {
		msg += "only in the second set: " + strings.Join(onlyInB, ", ") + "\n"
	}
	return msg
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
