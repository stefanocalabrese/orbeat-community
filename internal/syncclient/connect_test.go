package syncclient

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConnectLedgerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connect.json")

	// Absent file → empty ledger, no error.
	l, err := LoadConnectLedger(path)
	if err != nil {
		t.Fatalf("load absent: %v", err)
	}
	if len(l) != 0 {
		t.Fatalf("absent ledger not empty: %v", l)
	}

	l["codex"] = LedgerEntry{MCPPath: "/home/u/.codex/config.toml"}
	if err := saveConnectLedger(path, l); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadConnectLedger(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got["codex"].MCPPath != "/home/u/.codex/config.toml" {
		t.Fatalf("round-trip mismatch: %v", got)
	}
}

func TestRunConnectWritesDetectedToolsAndLedger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Only cursor + codex "installed".
	must(t, os.MkdirAll(filepath.Join(home, ".cursor"), 0o755))
	must(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o755))
	ledgerPath := filepath.Join(home, "connect.json")

	res, err := RunConnect(ConnectOptions{
		GatewayURL: "https://gw.example.com",
		LedgerPath: ledgerPath,
	})
	if err != nil {
		t.Fatalf("RunConnect: %v", err)
	}
	// gemini-cli/antigravity/windsurf absent → not written.
	if _, err := os.Stat(filepath.Join(home, ".gemini", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("wrote config for an uninstalled tool")
	}
	// cursor + codex present in results and changed.
	changed := map[string]bool{}
	for _, r := range res {
		changed[r.Tool] = r.Result.Changed
	}
	if !changed["cursor"] || !changed["codex"] {
		t.Fatalf("expected cursor+codex changed: %v", changed)
	}
	// Ledger records both.
	l, _ := LoadConnectLedger(ledgerPath)
	if _, ok := l["cursor"]; !ok {
		t.Fatal("ledger missing cursor")
	}
	if _, ok := l["codex"]; !ok {
		t.Fatal("ledger missing codex")
	}
}

func TestRunConnectRemoveStripsAndClearsLedger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	must(t, os.MkdirAll(filepath.Join(home, ".cursor"), 0o755))
	ledgerPath := filepath.Join(home, "connect.json")

	if _, err := RunConnect(ConnectOptions{GatewayURL: "https://gw", LedgerPath: ledgerPath}); err != nil {
		t.Fatal(err)
	}
	if _, err := RunConnect(ConnectOptions{Remove: true, LedgerPath: ledgerPath}); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, filepath.Join(home, ".cursor", "mcp.json"))
	if servers, ok := m["mcpServers"].(map[string]any); ok {
		if _, present := servers[orbeatServerName]; present {
			t.Fatal("remove did not strip the orbeat entry")
		}
	}
	l, _ := LoadConnectLedger(ledgerPath)
	if _, ok := l["cursor"]; ok {
		t.Fatal("ledger still records cursor after remove")
	}
}

func TestRunConnectToolsFilterUnknownErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, err := RunConnect(ConnectOptions{
		GatewayURL: "https://gw", LedgerPath: filepath.Join(home, "connect.json"),
		Only: []string{"nope"},
	})
	if err == nil {
		t.Fatal("expected error for unknown tool in --tools")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type fakeAdapter struct {
	name     string
	detect   bool
	writeErr error
	changed  bool
}

func (f fakeAdapter) Name() string { return f.name }
func (f fakeAdapter) Detect() bool { return f.detect }
func (f fakeAdapter) WriteMCP(string, bool) (Result, error) {
	if f.writeErr != nil {
		return Result{}, f.writeErr
	}
	return Result{Changed: f.changed, Path: "/x/" + f.name}, nil
}
func (f fakeAdapter) RemoveMCP() (Result, error) { return Result{Changed: true}, nil }
func (f fakeAdapter) AuthHint() string           { return "" }
func (f fakeAdapter) Caveat() string             { return "" }

func TestRunConnectPersistsLedgerOnPartialFailure(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "connect.json")
	ok := fakeAdapter{name: "ok", detect: true, changed: true}
	boom := fakeAdapter{name: "boom", detect: true, writeErr: errors.New("disk full")}
	_, err := RunConnect(ConnectOptions{
		GatewayURL: "https://gw", LedgerPath: ledgerPath,
		adapters: []ToolAdapter{ok, boom},
	})
	if err == nil {
		t.Fatal("expected the boom error")
	}
	l, _ := LoadConnectLedger(ledgerPath)
	if _, present := l["ok"]; !present {
		t.Fatal("ledger did not record the tool written before the failure")
	}
}

func TestRunConnectDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "connect.json")
	ok := fakeAdapter{name: "ok", detect: true, changed: true}
	res, err := RunConnect(ConnectOptions{
		GatewayURL: "https://gw", LedgerPath: ledgerPath, DryRun: true,
		adapters: []ToolAdapter{ok},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Result.Changed || res[0].Result.Note == "" {
		t.Fatalf("dry-run result wrong: %+v", res)
	}
	if _, statErr := os.Stat(ledgerPath); !os.IsNotExist(statErr) {
		t.Fatal("dry-run wrote the ledger")
	}
}

func TestRunConnectExcludeAndOnly(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "connect.json")
	a := fakeAdapter{name: "a", detect: true, changed: true}
	b := fakeAdapter{name: "b", detect: true, changed: true}
	// Only=[a,b], Exclude=[b] → only a processed.
	res, err := RunConnect(ConnectOptions{
		GatewayURL: "https://gw", LedgerPath: ledgerPath,
		Only: []string{"a", "b"}, Exclude: []string{"b"},
		adapters: []ToolAdapter{a, b},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Tool != "a" {
		t.Fatalf("exclude/only selection wrong: %+v", res)
	}
	// Unknown exclude name errors.
	if _, err := RunConnect(ConnectOptions{
		GatewayURL: "https://gw", LedgerPath: ledgerPath,
		Exclude: []string{"nope"}, adapters: []ToolAdapter{a},
	}); err == nil {
		t.Fatal("expected error for unknown --exclude tool")
	}
}
