package syncclient

import (
	"fmt"
	"os"
	"path/filepath"
)

// jsonAdapter is the shared implementation for tools whose MCP config is a JSON
// file with a top-level mcpServers object. entryKey is the URL field name the
// tool expects ("url" | "httpUrl" | "serverUrl"); extra holds any fixed extra
// fields (e.g. Gemini's authProviderType). The config file lives at
// <HOME>/<relDir...>/<fileName>, so Detect() and path() never drift.
type jsonAdapter struct {
	name     string
	relDir   []string // path components under HOME for Detect()
	fileName string   // config file basename inside relDir
	entryKey string
	extra    map[string]any
	authHint string
	caveat   string
}

func (a *jsonAdapter) Name() string { return a.name }

func (a *jsonAdapter) home() string { h, _ := os.UserHomeDir(); return h }

// homeErr resolves HOME, surfacing the error so a write never silently targets
// a CWD-relative path.
func (a *jsonAdapter) homeErr() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve home dir: %w", a.name, err)
	}
	return h, nil
}

func (a *jsonAdapter) dir() string {
	return filepath.Join(append([]string{a.home()}, a.relDir...)...)
}
func (a *jsonAdapter) path() string {
	return filepath.Join(append(append([]string{a.home()}, a.relDir...), a.fileName)...)
}

func (a *jsonAdapter) Detect() bool {
	if _, err := a.homeErr(); err != nil {
		return false // unresolvable HOME: never stat a CWD-relative config dir
	}
	fi, err := os.Stat(a.dir())
	return err == nil && fi.IsDir()
}

func (a *jsonAdapter) WriteMCP(gatewayURL string, managed bool) (Result, error) {
	if _, err := a.homeErr(); err != nil {
		return Result{}, err
	}
	entry := map[string]any{a.entryKey: gatewayMCPURL(gatewayURL)}
	for k, v := range a.extra {
		entry[k] = v
	}
	return writeJSONMCPEntry(a.path(), orbeatServerName, entry, managed)
}

func (a *jsonAdapter) RemoveMCP() (Result, error) {
	if _, err := a.homeErr(); err != nil {
		return Result{}, err
	}
	return removeJSONMCPEntry(a.path(), orbeatServerName)
}
func (a *jsonAdapter) AuthHint() string { return a.authHint }
func (a *jsonAdapter) Caveat() string   { return a.caveat }

func newCursorAdapter() *jsonAdapter {
	return &jsonAdapter{
		name: "cursor", relDir: []string{".cursor"}, fileName: "mcp.json",
		entryKey: "url",
		authHint: "Cursor: open Cursor and authorize the orbeat-gateway MCP server when prompted (Desktop).",
	}
}

func newGeminiCLIAdapter() *jsonAdapter {
	return &jsonAdapter{
		name: "gemini-cli", relDir: []string{".gemini"}, fileName: "settings.json",
		entryKey: "httpUrl", extra: map[string]any{"authProviderType": "dynamic_discovery"},
		authHint: "Gemini CLI: run `/mcp auth` to complete the one-time OAuth login.",
	}
}

func newAntigravityAdapter() *jsonAdapter {
	return &jsonAdapter{
		name: "antigravity", relDir: []string{".gemini", "config"}, fileName: "mcp_config.json",
		entryKey: "serverUrl",
		authHint: "Antigravity: it should auto-run OAuth on next start.",
		caveat:   "Antigravity: best-effort — verify the OAuth handshake against your Antigravity build (known runtime issue #25); fallback: static bearer header.",
	}
}

func newWindsurfAdapter() *jsonAdapter {
	return &jsonAdapter{
		name: "windsurf", relDir: []string{".codeium", "windsurf"}, fileName: "mcp_config.json",
		entryKey: "serverUrl",
		authHint: "Windsurf: authorize the orbeat-gateway server via the MCP panel (browser consent).",
		caveat:   "Windsurf: best-effort — needs a recent Windsurf/Devin-Desktop build for native remote OAuth; older builds require the `npx mcp-remote` stdio bridge.",
	}
}
