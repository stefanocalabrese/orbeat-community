package syncclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func TestWriteJSONMCPEntryPreservesOthersAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	// Pre-existing unrelated server.
	seed := `{"mcpServers":{"other":{"command":"x"}},"otherTopLevel":true}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"url": "https://gw/mcp"}

	r, err := writeJSONMCPEntry(path, orbeatServerName, entry, false)
	if err != nil || !r.Changed {
		t.Fatalf("first write: changed=%v err=%v", r.Changed, err)
	}
	m := readJSON(t, path)
	servers := m["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatal("pre-existing server 'other' was dropped")
	}
	if m["otherTopLevel"] != true {
		t.Fatal("unrelated top-level key was dropped")
	}
	if servers[orbeatServerName].(map[string]any)["url"] != "https://gw/mcp" {
		t.Fatalf("orbeat entry wrong: %v", servers[orbeatServerName])
	}

	// Idempotent: same write again → no change. managed=true (we wrote it).
	r2, err := writeJSONMCPEntry(path, orbeatServerName, entry, true)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if r2.Changed {
		t.Fatal("idempotent write reported a change")
	}
}

func TestWriteJSONMCPEntrySkipsForeignUnmanaged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	seed := `{"mcpServers":{"orbeat-gateway":{"url":"https://user-set/mcp"}}}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	// managed=false → a foreign orbeat-gateway entry must NOT be clobbered.
	r, err := writeJSONMCPEntry(path, orbeatServerName, map[string]any{"url": "https://gw/mcp"}, false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r.Changed {
		t.Fatal("clobbered a foreign orbeat-gateway entry")
	}
	if r.Note == "" {
		t.Fatal("expected a skip note")
	}
	if readJSON(t, path)["mcpServers"].(map[string]any)["orbeat-gateway"].(map[string]any)["url"] != "https://user-set/mcp" {
		t.Fatal("foreign entry was modified")
	}
}

func TestWriteJSONMCPEntryUnparseableSkips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte("{ not json // comment"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	r, err := writeJSONMCPEntry(path, orbeatServerName, map[string]any{"url": "https://gw/mcp"}, false)
	if err != nil {
		t.Fatalf("unparseable must not error: %v", err)
	}
	if r.Changed {
		t.Fatal("unparseable file was overwritten")
	}
	if !strings.Contains(r.Note, "orbeat-gateway") {
		t.Fatalf("expected a paste snippet in the note, got %q", r.Note)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("unparseable file content changed")
	}
}

func TestRemoveJSONMCPEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	seed := `{"mcpServers":{"other":{"command":"x"},"orbeat-gateway":{"url":"u"}}}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := removeJSONMCPEntry(path, orbeatServerName)
	if err != nil || !r.Changed {
		t.Fatalf("remove: changed=%v err=%v", r.Changed, err)
	}
	servers := readJSON(t, path)["mcpServers"].(map[string]any)
	if _, ok := servers["orbeat-gateway"]; ok {
		t.Fatal("orbeat entry not removed")
	}
	if _, ok := servers["other"]; !ok {
		t.Fatal("removal dropped an unrelated server")
	}
}

func TestTOMLManagedBlockAppendReplaceStrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := "# my codex config\nmodel = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"x\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	block := "[mcp_servers.orbeat-gateway]\nurl = \"https://gw/mcp\"\n"

	// Append.
	r, err := writeTOMLManagedBlock(path, block, false)
	if err != nil || !r.Changed {
		t.Fatalf("append: changed=%v err=%v", r.Changed, err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "model = \"gpt-5\"") || !strings.Contains(s, "[mcp_servers.other]") {
		t.Fatal("append clobbered the user's config")
	}
	if !strings.Contains(s, tomlBeginMarker) || !strings.Contains(s, "[mcp_servers.orbeat-gateway]") {
		t.Fatal("managed block not appended")
	}

	// Replace (idempotent content → no change; changed content → change).
	r2, _ := writeTOMLManagedBlock(path, block, true)
	if r2.Changed {
		t.Fatal("identical managed block reported a change")
	}
	block2 := "[mcp_servers.orbeat-gateway]\nurl = \"https://gw2/mcp\"\n"
	r3, _ := writeTOMLManagedBlock(path, block2, true)
	if !r3.Changed {
		t.Fatal("changed managed block not written")
	}
	data, _ = os.ReadFile(path)
	if strings.Count(string(data), tomlBeginMarker) != 1 {
		t.Fatal("replace duplicated the managed block")
	}
	if !strings.Contains(string(data), "gw2/mcp") {
		t.Fatal("replace did not update the block")
	}

	// Strip.
	r4, _ := removeTOMLManagedBlock(path)
	if !r4.Changed {
		t.Fatal("strip reported no change")
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), tomlBeginMarker) || strings.Contains(string(data), "orbeat-gateway") {
		t.Fatal("managed block not stripped")
	}
	if !strings.Contains(string(data), "[mcp_servers.other]") {
		t.Fatal("strip dropped the user's config")
	}
}

func TestWriteJSONMCPEntryNonObjectMcpServersSkips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	seed := `{"mcpServers":["x"],"editor":true}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := writeJSONMCPEntry(path, orbeatServerName, map[string]any{"url": "https://gw/mcp"}, false)
	if err != nil {
		t.Fatalf("non-object mcpServers must not error: %v", err)
	}
	if r.Changed {
		t.Fatal("overwrote a non-object mcpServers (data loss)")
	}
	if r.Note == "" {
		t.Fatal("expected a skip note")
	}
	after, _ := os.ReadFile(path)
	if string(after) != seed {
		t.Fatalf("file with non-object mcpServers was modified: %s", after)
	}
}

func TestWriteJSONMCPEntryPreservesFidelity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	// A big integer and an ampersand-bearing URL in the user's OTHER values.
	seed := `{"mcpServers":{"other":{"cmd":"x","bignum":123456789012345678901234567890}},"url":"https://a.com?x=1&y=2"}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := writeJSONMCPEntry(path, orbeatServerName, map[string]any{"url": "https://gw/mcp"}, false)
	if err != nil || !r.Changed {
		t.Fatalf("write: changed=%v err=%v", r.Changed, err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "https://a.com?x=1&y=2") {
		t.Fatalf("ampersand was HTML-escaped or value mangled:\n%s", s)
	}
	if strings.Contains(s, `\u0026`) || strings.Contains(s, "&amp;") {
		t.Fatalf("ampersand was escaped:\n%s", s)
	}
	if !strings.Contains(s, "123456789012345678901234567890") {
		t.Fatalf("big integer was mangled (UseNumber not honored):\n%s", s)
	}
	if _, ok := readJSON(t, path)["mcpServers"].(map[string]any)["other"]; !ok {
		t.Fatal("unrelated server 'other' was dropped")
	}
}

func TestWriteJSONMCPEntryInsertsWhenNoMcpServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	seed := `{"editor":true,"theme":"dark"}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := writeJSONMCPEntry(path, orbeatServerName, map[string]any{"url": "https://gw/mcp"}, false)
	if err != nil || !r.Changed {
		t.Fatalf("write: changed=%v err=%v", r.Changed, err)
	}
	m := readJSON(t, path)
	if m["editor"] != true || m["theme"] != "dark" {
		t.Fatal("existing top-level keys were dropped")
	}
	if m["mcpServers"].(map[string]any)[orbeatServerName].(map[string]any)["url"] != "https://gw/mcp" {
		t.Fatal("orbeat entry not inserted")
	}
}

func TestWriteJSONMCPEntryPreservesFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	seed := `{"mcpServers":{"other":{"command":"x"}}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeJSONMCPEntry(path, orbeatServerName, map[string]any{"url": "https://gw/mcp"}, false); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode widened to %o, want 600 (would leak other tools' keys)", fi.Mode().Perm())
	}
}

func TestRemoveJSONMCPEntryAbsentNoChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	seed := `{"mcpServers":{"other":{"command":"x"}}}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := removeJSONMCPEntry(path, orbeatServerName)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r.Changed {
		t.Fatal("remove reported a change when the entry was absent")
	}
	after, _ := os.ReadFile(path)
	if string(after) != seed {
		t.Fatal("no-op remove modified the file")
	}
}

func TestTOMLManagedBlockAppendToEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	block := "[mcp_servers.orbeat-gateway]\nurl = \"https://gw/mcp\"\n"
	r, err := writeTOMLManagedBlock(path, block, false)
	if err != nil || !r.Changed {
		t.Fatalf("append to empty: changed=%v err=%v", r.Changed, err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.HasPrefix(s, tomlBeginMarker) {
		t.Fatalf("block should start at the file head for an empty file:\n%q", s)
	}
	if !strings.Contains(s, "[mcp_servers.orbeat-gateway]") {
		t.Fatal("managed block not written")
	}
}

func TestRemoveTOMLManagedBlockAbsentNoChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	existing := "model = \"gpt-5\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := removeTOMLManagedBlock(path)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r.Changed {
		t.Fatal("remove reported a change when no managed block was present")
	}
	after, _ := os.ReadFile(path)
	if string(after) != existing {
		t.Fatal("no-op remove modified the file")
	}
}
