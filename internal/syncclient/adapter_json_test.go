package syncclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJSONAdapters(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		a        ToolAdapter
		dir      string // relative to home
		file     string // relative to home
		key      string // expected entry key
		caveated bool
	}{
		{newCursorAdapter(), ".cursor", ".cursor/mcp.json", "url", false},
		{newGeminiCLIAdapter(), ".gemini", ".gemini/settings.json", "httpUrl", false},
		{newAntigravityAdapter(), filepath.Join(".gemini", "config"), filepath.Join(".gemini", "config", "mcp_config.json"), "serverUrl", true},
		{newWindsurfAdapter(), filepath.Join(".codeium", "windsurf"), filepath.Join(".codeium", "windsurf", "mcp_config.json"), "serverUrl", true},
	}
	for _, tc := range cases {
		t.Run(tc.a.Name(), func(t *testing.T) {
			if tc.a.Detect() {
				t.Fatal("Detect true before dir exists")
			}
			if err := os.MkdirAll(filepath.Join(home, tc.dir), 0o755); err != nil {
				t.Fatal(err)
			}
			if !tc.a.Detect() {
				t.Fatal("Detect false after dir exists")
			}
			r, err := tc.a.WriteMCP("https://gw.example.com", false)
			if err != nil || !r.Changed {
				t.Fatalf("write: changed=%v err=%v", r.Changed, err)
			}
			m := readJSON(t, filepath.Join(home, tc.file))
			entry := m["mcpServers"].(map[string]any)[orbeatServerName].(map[string]any)
			if entry[tc.key] != "https://gw.example.com/mcp" {
				t.Fatalf("entry[%s] = %v", tc.key, entry[tc.key])
			}
			if (tc.a.Caveat() != "") != tc.caveated {
				t.Fatalf("caveat mismatch for %s: %q", tc.a.Name(), tc.a.Caveat())
			}
			rr, err := tc.a.RemoveMCP()
			if err != nil || !rr.Changed {
				t.Fatalf("remove: %v", err)
			}
		})
	}
}

func TestAllAdaptersRegistered(t *testing.T) {
	names := map[string]bool{}
	for _, a := range allAdapters() {
		names[a.Name()] = true
	}
	for _, want := range []string{"codex", "cursor", "gemini-cli", "antigravity", "windsurf"} {
		if !names[want] {
			t.Fatalf("adapter %q not registered", want)
		}
	}
}

func TestJSONAdapterWriteErrorsWhenNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	a := newCursorAdapter()
	if _, err := a.WriteMCP("https://gw", false); err == nil {
		t.Fatal("expected an error when HOME is unresolved (must not write CWD-relative)")
	}
	if _, err := a.RemoveMCP(); err == nil {
		t.Fatal("expected an error when HOME is unresolved")
	}
}
