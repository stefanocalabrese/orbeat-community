package syncclient

import (
	"fmt"
	"os"
	"path/filepath"
)

type codexAdapter struct{ home string }

func newCodexAdapter() *codexAdapter {
	home, _ := os.UserHomeDir()
	return &codexAdapter{home: home}
}

func (a *codexAdapter) Name() string { return "codex" }

func (a *codexAdapter) dir() string  { return filepath.Join(a.home, ".codex") }
func (a *codexAdapter) path() string { return filepath.Join(a.dir(), "config.toml") }

func (a *codexAdapter) Detect() bool {
	if a.home == "" {
		return false // unresolvable HOME: never stat a CWD-relative ".codex"
	}
	fi, err := os.Stat(a.dir())
	return err == nil && fi.IsDir()
}

func (a *codexAdapter) WriteMCP(gatewayURL string, managed bool) (Result, error) {
	if a.home == "" {
		return Result{}, fmt.Errorf("codex: cannot resolve home dir")
	}
	body := fmt.Sprintf("[mcp_servers.%s]\nurl = %q\noauth_resource = %q\n",
		orbeatServerName, gatewayMCPURL(gatewayURL), gatewayURL)
	return writeTOMLManagedBlock(a.path(), body, managed)
}

func (a *codexAdapter) RemoveMCP() (Result, error) {
	if a.home == "" {
		return Result{}, fmt.Errorf("codex: cannot resolve home dir")
	}
	return removeTOMLManagedBlock(a.path())
}

func (a *codexAdapter) AuthHint() string {
	return "Codex: run `codex mcp login orbeat-gateway` to complete the one-time OAuth login."
}

func (a *codexAdapter) Caveat() string { return "" }
