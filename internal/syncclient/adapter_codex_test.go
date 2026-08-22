package syncclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexAdapter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := newCodexAdapter()

	if a.Detect() {
		t.Fatal("Detect true before ~/.codex exists")
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !a.Detect() {
		t.Fatal("Detect false after ~/.codex exists")
	}

	r, err := a.WriteMCP("https://gw.example.com", false)
	if err != nil || !r.Changed {
		t.Fatalf("write: changed=%v err=%v", r.Changed, err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	s := string(data)
	if !strings.Contains(s, "[mcp_servers.orbeat-gateway]") ||
		!strings.Contains(s, `url = "https://gw.example.com/mcp"`) ||
		!strings.Contains(s, tomlBeginMarker) {
		t.Fatalf("codex config wrong:\n%s", s)
	}
	if a.Caveat() != "" {
		t.Fatal("codex should be first-class (no caveat)")
	}

	rr, err := a.RemoveMCP()
	if err != nil || !rr.Changed {
		t.Fatalf("remove: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if strings.Contains(string(data), "orbeat-gateway") {
		t.Fatal("codex entry not removed")
	}
}

// A user-authored [mcp_servers.orbeat-gateway] table with NO markers must never
// be clobbered — in either bare or quoted-key spelling (the old substring check
// missed the quoted form; go-toml parse-detection catches both).
func TestCodexAdapterSkipsForeignBareTable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table string
	}{
		{"bare", "[mcp_servers.orbeat-gateway]\nurl = \"https://user-set/mcp\"\n"},
		{"quoted-key", "[mcp_servers.\"orbeat-gateway\"]\nurl = \"https://user-set/mcp\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(home, ".codex", "config.toml")
			existing := "model = \"gpt-5\"\n\n" + tc.table
			if err := os.WriteFile(p, []byte(existing), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := newCodexAdapter().WriteMCP("https://gw.example.com", false)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if r.Changed {
				t.Fatal("clobbered a foreign [mcp_servers.orbeat-gateway] table")
			}
			if r.Note == "" {
				t.Fatal("expected a skip note")
			}
			after, _ := os.ReadFile(p)
			if string(after) != existing {
				t.Fatalf("foreign config was modified:\n%s", after)
			}
		})
	}
}

func TestCodexAdapterSkipsInvalidTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".codex", "config.toml")
	broken := "broken = = toml ][\n"
	if err := os.WriteFile(p, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := newCodexAdapter().WriteMCP("https://gw.example.com", false)
	if err != nil {
		t.Fatalf("unparseable TOML must not error: %v", err)
	}
	if r.Changed {
		t.Fatal("unparseable TOML was overwritten")
	}
	if r.Note == "" {
		t.Fatal("expected a skip note")
	}
	after, _ := os.ReadFile(p)
	if string(after) != broken {
		t.Fatal("unparseable TOML content changed")
	}
}

func TestCodexAdapterWriteErrorsWhenNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	a := newCodexAdapter()
	if _, err := a.WriteMCP("https://gw", false); err == nil {
		t.Fatal("expected an error when HOME is unresolved (must not write CWD-relative)")
	}
	if _, err := a.RemoveMCP(); err == nil {
		t.Fatal("expected an error when HOME is unresolved")
	}
}
