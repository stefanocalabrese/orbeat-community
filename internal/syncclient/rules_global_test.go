package syncclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGlobalRulesLandInUserLevelFiles pins the whole point: a global rule
// reaches the developer's user-level instruction file and NOT the registered
// project, while a project rule does the opposite. Asserting both directions in
// one test is what makes it discriminating: a reconciler that wrote every rule
// everywhere would satisfy either half alone.
func TestGlobalRulesLandInUserLevelFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	proj := t.TempDir()

	arts := []Artifact{
		{Type: "rule", Name: "project-rule", Content: "PROJECT-BODY"},
		{Type: "rule", Name: "global-rule", Content: "GLOBAL-BODY", RuleScope: "global"},
	}
	if _, err := ReconcileRules(claudeDir, []Project{{Path: proj}}, arts, nil); err != nil {
		t.Fatal(err)
	}

	globalFile := readFile(t, filepath.Join(claudeDir, "CLAUDE.md"))
	if !strings.Contains(globalFile, "GLOBAL-BODY") {
		t.Fatalf("the global rule never reached ~/.claude/CLAUDE.md:\n%s", globalFile)
	}
	if strings.Contains(globalFile, "PROJECT-BODY") {
		t.Fatalf("a project rule leaked into the user-level file:\n%s", globalFile)
	}

	projFile := readFile(t, filepath.Join(proj, "AGENTS.md"))
	if !strings.Contains(projFile, "PROJECT-BODY") {
		t.Fatalf("the project rule never reached the project:\n%s", projFile)
	}
	if strings.Contains(projFile, "GLOBAL-BODY") {
		t.Fatalf("a global rule was ALSO written into a project, so it now applies twice:\n%s", projFile)
	}
}

// TestGlobalRulesWriteCodexOnlyWhenInstalled pins that the client never creates
// a tool's home directory. Provisioning an installation that does not exist
// would leave a file for an agent nobody runs, and `orbeat-sync connect`
// already treats a missing tool as not installed rather than as something to
// set up.
func TestGlobalRulesWriteCodexOnlyWhenInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	arts := []Artifact{{Type: "rule", Name: "g", Content: "GLOBAL-BODY", RuleScope: "global"}}

	if _, err := ReconcileRules(claudeDir, nil, arts, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("~/.codex was created for a tool that is not installed (stat err: %v)", err)
	}

	// Install it, and the next sync manages its AGENTS.md too.
	must(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o755))
	if _, err := ReconcileRules(claudeDir, nil, arts, nil); err != nil {
		t.Fatal(err)
	}
	codexFile := readFile(t, filepath.Join(home, ".codex", "AGENTS.md"))
	if !strings.Contains(codexFile, "GLOBAL-BODY") {
		t.Fatalf("an installed Codex did not receive the global rule:\n%s", codexFile)
	}
	// AGENTS.override.md is the developer's escape hatch from org instructions;
	// writing org instructions into it would take the hatch away.
	if _, err := os.Stat(filepath.Join(home, ".codex", "AGENTS.override.md")); !os.IsNotExist(err) {
		t.Fatal("AGENTS.override.md must never be written: it is the developer's override of org rules")
	}
}

// TestGlobalRulesStrippedOnDeEntitlement is the withdrawal half. A rule that
// stops being entitled has to leave the user-level file, or an instruction
// could be published once and never taken back, which is worse at global scope
// than at project scope because it applies to everything the developer does.
func TestGlobalRulesStrippedOnDeEntitlement(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	must(t, os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"), []byte("# My own notes\nkeep me\n"), 0o644))

	arts := []Artifact{{Type: "rule", Name: "g", Content: "GLOBAL-BODY", RuleScope: "global"}}
	if _, err := ReconcileRules(claudeDir, nil, arts, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, filepath.Join(claudeDir, "CLAUDE.md")), "GLOBAL-BODY") {
		t.Fatal("precondition failed: the global rule never landed")
	}

	res, err := ReconcileRules(claudeDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stripped == 0 {
		t.Fatalf("de-entitling every global rule must strip the block, got %+v", res)
	}
	after := readFile(t, filepath.Join(claudeDir, "CLAUDE.md"))
	if strings.Contains(after, "GLOBAL-BODY") {
		t.Fatalf("the global rule survived de-entitlement:\n%s", after)
	}
	if !strings.Contains(after, "keep me") {
		t.Fatalf("the developer's own content was destroyed by the strip:\n%s", after)
	}
}

// TestGlobalRulesLedgerRejectsATamperedPath pins that the strip pass will not
// act on an arbitrary path someone writes into the manifest, which is a
// user-editable file on disk. The entry names a real file this test owns, so a
// reconciler that trusted the ledger would visibly destroy it.
func TestGlobalRulesLedgerRejectsATamperedPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))

	victim := filepath.Join(t.TempDir(), "id_rsa")
	must(t, os.WriteFile(victim, []byte("PRIVATE KEY"), 0o600))

	m, err := loadManifest(claudeDir)
	must(t, err)
	m.Globals = []string{victim}
	must(t, saveManifest(claudeDir, m, nil))

	res, err := ReconcileRules(claudeDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if body := readFile(t, victim); body != "PRIVATE KEY" {
		t.Fatalf("the strip pass touched a file the ledger should never have named: %q", body)
	}
	joined := strings.Join(res.Warnings, " ")
	if !strings.Contains(joined, "malformed global rules ledger entry") {
		t.Fatalf("a rejected ledger entry must be reported, got warnings %v", res.Warnings)
	}
}

// TestGlobalRulesLedgerRejectsATrustedFilenameInAnUntrustedDirectory closes
// the gap TestGlobalRulesLedgerRejectsATamperedPath's fixture cannot see:
// that test's victim is named "id_rsa", which validGlobalRulesPath already
// refuses on FILENAME alone. This victim is named exactly "CLAUDE.md" — a
// name the shape check accepts — sitting in a directory that is neither
// claudeDir nor the Codex home. Before this fix, validGlobalRulesPath's shape
// check (absolute, clean, filename in {CLAUDE.md, AGENTS.md}) was the ONLY
// gate the strip pass applied: stripGlobalRules then opened a containment
// root at filepath.Dir(path) — the untrusted ledger entry itself — so this
// directory passed straight through and lost its block. Mirrors
// trustedSeedBoundary's fix for seeds (seed.go) and the identical rules-side
// fix above (TestReconcileRulesRefusesToStripAnUnregisteredProjectLedgerEntry).
func TestGlobalRulesLedgerRejectsATrustedFilenameInAnUntrustedDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))

	elsewhere := t.TempDir()
	victim := filepath.Join(elsewhere, "CLAUDE.md")
	content := "precious dev notes\n\n" + renderRulesBlock("GLOBAL-BODY")
	must(t, os.WriteFile(victim, []byte(content), 0o644))

	m, err := loadManifest(claudeDir)
	must(t, err)
	m.Globals = []string{victim}
	must(t, saveManifest(claudeDir, m, nil))

	res, err := ReconcileRules(claudeDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, victim); got != content {
		t.Fatalf("a directory this client never writes to must not be touched:\nwant: %q\ngot:  %q", content, got)
	}
	if res.Stripped != 0 {
		t.Fatalf("nothing outside the trusted global targets may be stripped, got Stripped=%d", res.Stripped)
	}
	named := false
	for _, w := range res.Warnings {
		if strings.Contains(w, victim) {
			named = true
		}
	}
	if !named {
		t.Fatalf("the skip must be surfaced and must name the path, got warnings=%v", res.Warnings)
	}

	m2, err := loadManifest(claudeDir)
	must(t, err)
	found := false
	for _, p := range m2.Globals {
		if p == victim {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refused entry must stay in the ledger; globals=%v", m2.Globals)
	}
}
