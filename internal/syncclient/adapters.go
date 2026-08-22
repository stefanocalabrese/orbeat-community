package syncclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// orbeatServerName is the MCP server name orbeat-sync owns in every tool config.
const orbeatServerName = "orbeat-gateway"

const (
	tomlBeginMarker = "# >>> orbeat-sync managed (do not edit) >>>"
	tomlEndMarker   = "# <<< orbeat-sync managed <<<"
)

// Result summarizes one adapter operation.
type Result struct {
	Changed bool
	Path    string
	Note    string // non-empty when skipped (foreign entry / unparseable) or informational
}

// ToolAdapter encapsulates one AI tool's local MCP config surface. Slice B will
// add WriteRules/RemoveRules; this slice is MCP-only.
type ToolAdapter interface {
	Name() string // "codex", "cursor", ...
	Detect() bool // is the tool installed (config dir exists)?
	WriteMCP(gatewayURL string, managed bool) (Result, error)
	RemoveMCP() (Result, error)
	AuthHint() string // one-time consent step to print after a change
	Caveat() string   // "" for first-class; a warning for best-effort tools
}

// allAdapters returns the built-in adapters in a stable order. The adapter
// constructors themselves live in adapter_codex.go / adapter_json.go.
func allAdapters() []ToolAdapter {
	return []ToolAdapter{
		newCodexAdapter(),
		newCursorAdapter(),
		newGeminiCLIAdapter(),
		newAntigravityAdapter(),
		newWindsurfAdapter(),
	}
}

// gatewayMCPURL is the MCP endpoint written into each tool: <gatewayURL>/mcp.
func gatewayMCPURL(gatewayURL string) string {
	return strings.TrimRight(gatewayURL, "/") + "/mcp"
}

// existingPerm returns the current file's permission bits, or def when the file
// does not exist. Lets a write preserve a user's tighter mode (e.g. a 0600
// config holding other tools' API keys is not widened to 0644).
func existingPerm(path string, def os.FileMode) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return def
}

// --- JSON config helpers (Cursor / Gemini CLI / Antigravity / Windsurf) ---

// decodeJSONDoc parses raw into a map, preserving numbers verbatim (UseNumber)
// so a user's large integers in OTHER values survive the round-trip unmangled.
func decodeJSONDoc(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	m := map[string]any{}
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// encodeJSONDoc serializes m with HTML escaping disabled so <, > and & in a
// user's OTHER values are written literally, not as < etc. The trailing
// newline that Encode appends is intentional.
func encodeJSONDoc(m map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil { // Encode appends a trailing newline
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeJSONMCPEntry merges entry into path's mcpServers[name], preserving every
// other key. managed reports whether the connect ledger recorded a prior orbeat
// write to this tool; when false and a name entry already exists, the entry is
// treated as user-authored and left untouched (skip + note). An unparseable
// existing file, or one whose top-level mcpServers is not an object, is never
// overwritten — the note carries a paste snippet.
func writeJSONMCPEntry(path, name string, entry map[string]any, managed bool) (Result, error) {
	res := Result{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("read %s: %w", path, err)
	}
	doc := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		d, err := decodeJSONDoc(raw)
		if err != nil {
			res.Note = fmt.Sprintf("skipped %s: not valid JSON — add manually under mcpServers:\n%s", path, jsonSnippet(name, entry))
			return res, nil // never clobber an unparseable file
		}
		doc = d
	}
	var servers map[string]any
	if v, present := doc["mcpServers"]; present {
		m, ok := v.(map[string]any)
		if !ok {
			res.Note = fmt.Sprintf("skipped %s: top-level \"mcpServers\" is not an object — add manually under mcpServers:\n%s", path, jsonSnippet(name, entry))
			return res, nil // never clobber a non-object mcpServers
		}
		servers = m
	}
	if servers == nil {
		servers = map[string]any{}
	}
	if _, ok := servers[name]; ok && !managed {
		res.Note = fmt.Sprintf("skipped %s: a %q entry already exists and was not written by orbeat-sync", path, name)
		return res, nil
	}
	// Idempotency: compare marshaled forms.
	if cur, ok := servers[name]; ok && jsonEqual(cur, entry) {
		return res, nil // no change
	}
	servers[name] = entry
	doc["mcpServers"] = servers
	out, err := encodeJSONDoc(doc)
	if err != nil {
		return res, fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := writeFileAtomic(path, out, existingPerm(path, 0o644)); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// removeJSONMCPEntry deletes mcpServers[name] from path, leaving all else intact.
func removeJSONMCPEntry(path, name string) (Result, error) {
	res := Result{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, fmt.Errorf("read %s: %w", path, err)
	}
	doc, err := decodeJSONDoc(raw)
	if err != nil {
		// Unparseable: we cannot safely strip the entry, and it may still be in the
		// file. Surface a Note so the caller keeps the ledger entry (a silent drop
		// would make the orbeat entry unmanageable forever) — the entry is deleted
		// only on a confirmed removal or confirmed absence (Note == "").
		res.Note = fmt.Sprintf("skipped %s: not valid JSON — remove the %q entry manually", path, name)
		return res, nil
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		return res, nil // absent or non-object — nothing to remove, leave untouched
	}
	if _, ok := servers[name]; !ok {
		return res, nil
	}
	delete(servers, name)
	doc["mcpServers"] = servers
	out, err := encodeJSONDoc(doc)
	if err != nil {
		return res, fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := writeFileAtomic(path, out, existingPerm(path, 0o644)); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

func jsonSnippet(name string, entry map[string]any) string {
	b, _ := json.MarshalIndent(map[string]any{name: entry}, "  ", "  ")
	return "  " + string(b)
}

// --- TOML managed-block helpers (Codex) ---

// lineAnchoredIndex returns the byte index of marker when it starts a line
// (file start or right after a '\n'), else -1. Prevents a marker embedded in a
// user's string/comment value from being mistaken for a managed-block boundary.
func lineAnchoredIndex(s, marker string) int {
	if strings.HasPrefix(s, marker) {
		return 0
	}
	if i := strings.Index(s, "\n"+marker); i >= 0 {
		return i + 1
	}
	return -1
}

// findManagedBlock locates the marker-delimited block. corrupt=true means a
// begin marker with no matching end (caller aborts). end is just past the
// end-marker's line (including its newline).
func findManagedBlock(content string) (begin, end int, found, corrupt bool) {
	b := lineAnchoredIndex(content, tomlBeginMarker)
	if b < 0 {
		return 0, 0, false, false
	}
	e := lineAnchoredIndex(content[b:], tomlEndMarker)
	if e < 0 {
		return 0, 0, false, true
	}
	e = b + e
	if nl := strings.IndexByte(content[e:], '\n'); nl < 0 {
		end = len(content)
	} else {
		end = e + nl + 1
	}
	return b, end, true, false
}

// tomlHasOrbeatTable reports whether a parsed TOML doc already declares an
// [mcp_servers.orbeat-gateway] table (in either bare or quoted-key spelling —
// both decode to the same nested key).
func tomlHasOrbeatTable(doc map[string]any) bool {
	servers, ok := doc["mcp_servers"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = servers[orbeatServerName]
	return ok
}

// writeTOMLManagedBlock inserts/updates a marker-delimited block in path. body is
// the TOML between the markers (e.g. the [mcp_servers.orbeat-gateway] table). If
// a marker block exists it is replaced; else if a bare [mcp_servers.orbeat-gateway]
// table exists (user-authored) and !managed, the file is left untouched; else the
// block is appended. The rest of the file is preserved verbatim, and the result
// is re-validated as TOML before any write (spec §3.3).
func writeTOMLManagedBlock(path, body string, managed bool) (Result, error) {
	res := Result{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("read %s: %w", path, err)
	}
	content := string(raw)
	block := tomlBeginMarker + "\n" + strings.TrimRight(body, "\n") + "\n" + tomlEndMarker + "\n"

	begin, end, found, corrupt := findManagedBlock(content)
	var updated string
	switch {
	case corrupt:
		// A begin marker with no end is a skip+Note, mirroring the unparseable-TOML
		// branch below — never an error. An error here would abort the whole connect
		// run (RunConnect's per-adapter loop), taking down every other tool over one
		// tool's hand-mangled config.
		res.Note = fmt.Sprintf("skipped %s: corrupt managed block (begin marker without end) — repair it manually:\n%s", path, strings.TrimRight(body, "\n"))
		return res, nil
	case found:
		updated = content[:begin] + block + content[end:]
	default:
		if strings.TrimSpace(content) != "" {
			var doc map[string]any
			if err := toml.Unmarshal([]byte(content), &doc); err != nil {
				res.Note = fmt.Sprintf("skipped %s: not valid TOML — add manually:\n%s", path, strings.TrimRight(body, "\n"))
				return res, nil // never touch an unparseable/broken config
			}
			if !managed && tomlHasOrbeatTable(doc) {
				res.Note = fmt.Sprintf("skipped %s: an [mcp_servers.%s] table already exists and was not written by orbeat-sync", path, orbeatServerName)
				return res, nil
			}
		}
		sep := ""
		if strings.TrimSpace(content) != "" {
			if !strings.HasSuffix(content, "\n") {
				sep = "\n"
			}
			sep += "\n"
		}
		updated = content + sep + block
	}
	if updated == content {
		return res, nil
	}
	// Spec §3.3 backstop: the result MUST parse as TOML, else abort without writing.
	if err := toml.Unmarshal([]byte(updated), &map[string]any{}); err != nil {
		res.Note = fmt.Sprintf("skipped %s: the update would produce invalid TOML (left unchanged)", path)
		return res, nil
	}
	if err := writeFileAtomic(path, []byte(updated), existingPerm(path, 0o644)); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// removeTOMLManagedBlock strips the marker-delimited block from path.
func removeTOMLManagedBlock(path string) (Result, error) {
	res := Result{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, fmt.Errorf("read %s: %w", path, err)
	}
	content := string(raw)
	begin, end, found, corrupt := findManagedBlock(content)
	if corrupt {
		// A begin marker with no end: a partial orbeat block is still on disk and we
		// cannot safely splice it out. Note it (so the caller preserves the ledger
		// entry) rather than reporting a clean removal.
		res.Note = fmt.Sprintf("skipped %s: corrupt managed block (begin marker without end) — remove the orbeat entry manually", path)
		return res, nil
	}
	if !found {
		return res, nil // no managed block: the orbeat entry is confirmed absent
	}
	// also drop a single blank separator line we may have inserted before the block
	pre := strings.TrimRight(content[:begin], "\n")
	if pre != "" {
		pre += "\n"
	}
	updated := pre + content[end:]
	if updated == content {
		return res, nil
	}
	if err := writeFileAtomic(path, []byte(updated), existingPerm(path, 0o644)); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}
