package syncclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeRulesAppendUpdateIdempotentStrip(t *testing.T) {
	// Append into a file with the dev's own content.
	existing := "# My project\n\nHand-written guidance.\n"
	merged, changed := mergeRules(existing, "## r1\n\nbody one\n")
	if !changed {
		t.Fatal("append: expected change")
	}
	if !strings.Contains(merged, "# My project") || !strings.Contains(merged, "Hand-written guidance.") {
		t.Fatal("append clobbered the dev's content")
	}
	if !strings.Contains(merged, "<!-- ORBEAT-RULES:BEGIN") || !strings.Contains(merged, "## r1") {
		t.Fatal("managed block not appended")
	}

	// Idempotent: same body → no change.
	if _, c := mergeRules(merged, "## r1\n\nbody one\n"); c {
		t.Fatal("identical body reported a change")
	}

	// Update in place: changed body → change, single block, dev content intact.
	updated, c := mergeRules(merged, "## r1\n\nbody TWO\n")
	if !c {
		t.Fatal("changed body not written")
	}
	if strings.Count(updated, "<!-- ORBEAT-RULES:BEGIN") != 1 {
		t.Fatal("update duplicated the block")
	}
	if !strings.Contains(updated, "body TWO") || !strings.Contains(updated, "# My project") {
		t.Fatal("update lost content")
	}

	// Strip: block gone, dev content preserved.
	stripped, nStrip := stripRules(updated)
	if nStrip == 0 {
		t.Fatal("strip: expected change")
	}
	if strings.Contains(stripped, "ORBEAT-RULES") {
		t.Fatal("block not stripped")
	}
	if !strings.Contains(stripped, "# My project") {
		t.Fatal("strip dropped the dev's content")
	}
	if _, n2 := stripRules(stripped); n2 != 0 {
		t.Fatal("strip on absent block reported a change")
	}
}

func TestMergeRulesFileSkipsMalformedMarkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	// orphan BEGIN (no END) above dev content
	orphan := "<!-- ORBEAT-RULES:BEGIN sha=abc123abc123 x -->\n\nimportant dev note\n"
	must(t, os.WriteFile(path, []byte(orphan), 0o644))
	r, err := openRooted(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	changed, warning, err := mergeRulesFile(r, path, "## r\n\nbody\n")
	if err != nil {
		t.Fatal(err)
	}
	if changed || warning == "" {
		t.Fatalf("expected skip+warning, got changed=%v warning=%q", changed, warning)
	}
	after, _ := os.ReadFile(path)
	if string(after) != orphan {
		t.Fatalf("malformed file was modified:\n%s", after)
	}
}

func TestMergeRulesUpdatePreservesContentAboveAndBelow(t *testing.T) {
	existing := "# top\n\n<!-- ORBEAT-RULES:BEGIN sha=" + rulesHash("## r\n\nold\n") +
		" — managed -->\n## r\n\nold\n<!-- ORBEAT-RULES:END -->\n\n# bottom dev section\n"
	out, changed := mergeRules(existing, "## r\n\nnew\n")
	if !changed {
		t.Fatal("expected change")
	}
	if !strings.Contains(out, "# top") || !strings.Contains(out, "# bottom dev section") {
		t.Fatalf("update lost surrounding content:\n%s", out)
	}
	if strings.Count(out, "ORBEAT-RULES:BEGIN") != 1 || !strings.Contains(out, "new") {
		t.Fatalf("update wrong:\n%s", out)
	}
}

func TestStripRulesRemovesAllBlocks(t *testing.T) {
	block := renderRulesBlock("## r\n\nx\n")
	existing := "# dev\n\n" + block + "\nmiddle\n\n" + block // hand-copied duplicate
	out, n := stripRules(existing)
	if n != 2 {
		t.Fatalf("expected 2 blocks stripped, got %d", n)
	}
	if strings.Contains(out, "ORBEAT-RULES") {
		t.Fatalf("blocks remain:\n%s", out)
	}
	if !strings.Contains(out, "# dev") || !strings.Contains(out, "middle") {
		t.Fatalf("strip dropped dev content:\n%s", out)
	}
}

func TestRenderRulesBodyEdgeCases(t *testing.T) {
	one := renderRulesBody([]Artifact{{Name: "solo", Content: "no newline"}})
	if !strings.Contains(one, "## solo\n\nno newline\n") {
		t.Fatalf("single/no-newline wrong: %q", one)
	}
	empty := renderRulesBody([]Artifact{{Name: "e", Content: ""}})
	if !strings.Contains(empty, "## e\n\n\n") {
		t.Fatalf("empty content wrong: %q", empty)
	}
}

func TestReconcileRulesWarnsOnDeadProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	dead := filepath.Join(home, "nonexistent-proj")
	res, err := ReconcileRules(claudeDir, []string{dead}, []Artifact{{Type: "rule", Name: "r", Content: "x"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a dead-project warning")
	}
}

func TestReconcileRulesPreservesFileMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	proj := t.TempDir()
	ap := filepath.Join(proj, "AGENTS.md")
	must(t, os.WriteFile(ap, []byte("# dev\n"), 0o600))
	if _, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "x"}}, nil); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(ap)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode widened to %v", fi.Mode().Perm())
	}
}

func TestReconcileRulesWritesProjectsAndStrips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	proj := t.TempDir()

	rules := []Artifact{
		{Type: "rule", Name: "b-rule", Content: "second"},
		{Type: "rule", Name: "a-rule", Content: "first"},
		{Type: "skill", Name: "ignore-me", Content: "not a rule"},
	}

	res, err := ReconcileRules(claudeDir, []string{proj}, rules, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Written == 0 {
		t.Fatalf("expected writes, got %+v", res)
	}
	agents, _ := os.ReadFile(filepath.Join(proj, "AGENTS.md"))
	s := string(agents)
	// ordered by name: a-rule before b-rule
	if !strings.Contains(s, "## a-rule") || !strings.Contains(s, "## b-rule") ||
		strings.Index(s, "## a-rule") > strings.Index(s, "## b-rule") {
		t.Fatalf("rules not rendered in name order:\n%s", s)
	}
	claude, _ := os.ReadFile(filepath.Join(proj, "CLAUDE.md"))
	if !strings.Contains(string(claude), "@AGENTS.md") || !strings.Contains(string(claude), "ORBEAT-RULES:BEGIN") {
		t.Fatalf("CLAUDE.md import block wrong:\n%s", claude)
	}

	// Idempotent re-run.
	res2, _ := ReconcileRules(claudeDir, []string{proj}, rules, nil)
	if res2.Written != 0 {
		t.Fatalf("re-run should be unchanged, got %+v", res2)
	}

	// De-entitle everything → both blocks stripped.
	res3, err := ReconcileRules(claudeDir, []string{proj}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Stripped == 0 {
		t.Fatal("expected strips on de-entitlement")
	}
	agents, _ = os.ReadFile(filepath.Join(proj, "AGENTS.md"))
	if strings.Contains(string(agents), "ORBEAT-RULES") {
		t.Fatal("AGENTS.md block not stripped")
	}
}

func TestStripProjectRules(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	proj := t.TempDir()
	if _, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "x"}}, nil); err != nil {
		t.Fatal(err)
	}
	n, err := StripProjectRules(claudeDir, proj)
	if err != nil || n == 0 {
		t.Fatalf("StripProjectRules: n=%d err=%v", n, err)
	}
	agents, _ := os.ReadFile(filepath.Join(proj, "AGENTS.md"))
	if strings.Contains(string(agents), "ORBEAT-RULES") {
		t.Fatal("block not stripped by StripProjectRules")
	}
}

// One project's failure does not starve the others.
func TestReconcileRulesWriteFailureIsolated(t *testing.T) {
	claudeDir := t.TempDir()
	good := t.TempDir()
	bad := t.TempDir()
	// A directory at AGENTS.md makes mergeRulesFile's READ fail (EISDIR).
	if err := os.MkdirAll(filepath.Join(bad, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := ReconcileRules(claudeDir, []string{good, bad}, []Artifact{{Type: "rule", Name: "r", Content: "do the thing"}}, nil)

	if err != nil {
		t.Fatalf("per-project I/O failure must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("want exactly 1 failure, got %v", res.Failures)
	}
	if !strings.Contains(read(t, filepath.Join(good, "AGENTS.md")), "ORBEAT-RULES:BEGIN") {
		t.Fatal("the healthy project must still get its rules block")
	}
}

// THE LOAD-BEARING CASE: a strip that fails keeps its ledger entry, so a later
// run retries instead of orphaning the block forever (no fs-scan net for rules).
func TestReconcileRulesStripFailurePreservesLedger(t *testing.T) {
	claudeDir := t.TempDir()
	proj := t.TempDir()
	if _, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "x"}}, nil); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	// Replace AGENTS.md with a directory so the strip's read fails (EISDIR).
	if err := os.Remove(filepath.Join(proj, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(proj, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := ReconcileRules(claudeDir, []string{proj}, nil, nil) // de-entitle
	if err != nil {
		t.Fatalf("strip I/O failure must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("want exactly 1 failure, got %v", res.Failures)
	}
	m, err := loadManifest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	clean := filepath.Clean(proj)
	found := false
	for _, p := range m.Rules {
		if filepath.Clean(p) == clean {
			found = true
		}
	}
	if !found {
		t.Fatalf("strip-failed project must stay in the Rules ledger; ledger=%v", m.Rules)
	}
}

// A write-failed project that was previously in the ledger keeps its entry.
func TestReconcileRulesWriteFailurePreservesLedger(t *testing.T) {
	claudeDir := t.TempDir()
	proj := t.TempDir()
	// Run 1: establish the block + ledger entry.
	if _, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "v1"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(proj, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(proj, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Run 2: still entitled but the project now fails.
	res, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "v2"}}, nil)

	if err != nil {
		t.Fatalf("must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("want 1 failure, got %v", res.Failures)
	}
	m, err := loadManifest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Rules) != 1 || filepath.Clean(m.Rules[0]) != filepath.Clean(proj) {
		t.Fatalf("write-failed project must keep its ledger entry; ledger=%v", m.Rules)
	}
}

// A genuine WRITE-branch failure (parent dir unwritable -> CreateTemp fails).
func TestReconcileRulesAtomicWriteFailureIsolated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	claudeDir := t.TempDir()
	proj := t.TempDir()
	if _, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "v1"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(proj, 0o500); err != nil { // readable, not writable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(proj, 0o755) })
	res, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "v2"}}, nil)
	// changed -> write attempted
	if err != nil {
		t.Fatalf("write failure must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("want 1 failure, got %v", res.Failures)
	}
	if !strings.Contains(res.Failures[0], "create temp") {
		t.Fatalf("expected the atomic-write CreateTemp branch, got %q", res.Failures[0])
	}
}

// An unsafe rule name is fatal.
func TestReconcileRulesUnsafeNameIsFatal(t *testing.T) {
	_, err := ReconcileRules(t.TempDir(), nil, []Artifact{{Type: "rule", Name: "../evil", Content: "x"}}, nil)
	if err == nil || !isFatal(err) {
		t.Fatalf("unsafe rule name must be fatal, got %v", err)
	}
}

// A registered project that is momentarily unstattable must keep its ledger
// entry — an unmounted volume is indistinguishable from a deleted dir, and
// dropping the entry would strand its block forever (rules has no fs-scan net).
func TestReconcileRulesMissingProjectPreservesLedger(t *testing.T) {
	claudeDir := t.TempDir()
	parent := t.TempDir()
	proj := filepath.Join(parent, "myproj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	art := []Artifact{{Type: "rule", Name: "r", Content: "x"}}
	if _, err := ReconcileRules(claudeDir, []string{proj}, art, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate a transient outage: move the whole project dir away.
	away := filepath.Join(parent, "moved")
	if err := os.Rename(proj, away); err != nil {
		t.Fatal(err)
	}
	res, err := ReconcileRules(claudeDir, []string{proj}, art, nil)
	if err != nil {
		t.Fatalf("a missing project must not be fatal: %v", err)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("want the missing-project warning, got %v", res.Warnings)
	}
	m, err := loadManifest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Rules) != 1 || filepath.Clean(m.Rules[0]) != filepath.Clean(proj) {
		t.Fatalf("a transiently-missing project must keep its ledger entry; ledger=%v", m.Rules)
	}
	// Recovery: the volume comes back and the rule is de-entitled -> stripped.
	if err := os.Rename(away, proj); err != nil {
		t.Fatal(err)
	}
	res2, err := ReconcileRules(claudeDir, []string{proj}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Stripped == 0 {
		t.Fatal("after recovery the preserved entry must let the strip run (self-heal)")
	}
	if strings.Contains(read(t, filepath.Join(proj, "AGENTS.md")), "ORBEAT-RULES") {
		t.Fatal("block should have been stripped after recovery")
	}
}

// A partial write (AGENTS.md lands, CLAUDE.md fails) on a FIRST-EVER sync must
// still record the ledger entry — the block is already on disk and the ledger is
// the only record of it.
func TestReconcileRulesPartialWriteRecordsLedger(t *testing.T) {
	claudeDir := t.TempDir()
	proj := t.TempDir()
	// CLAUDE.md as a directory: AGENTS.md is written first and succeeds, then
	// CLAUDE.md's merge fails -> partial success, no prior ledger entry.
	if err := os.MkdirAll(filepath.Join(proj, "CLAUDE.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "x"}}, nil)

	if err != nil {
		t.Fatalf("partial write must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("want 1 failure, got %v", res.Failures)
	}
	// The block IS on disk...
	if !strings.Contains(read(t, filepath.Join(proj, "AGENTS.md")), "ORBEAT-RULES:BEGIN") {
		t.Fatal("precondition: AGENTS.md should have been written before CLAUDE.md failed")
	}
	// ...so the ledger MUST know about it, or it can never be stripped.
	m, err := loadManifest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Rules) != 1 || filepath.Clean(m.Rules[0]) != filepath.Clean(proj) {
		t.Fatalf("a partially-written project must be in the ledger; ledger=%v", m.Rules)
	}
}

// S1-class finding (same class as the seed.go audit finding, surfaced while
// fixing the seed side): only the WRITE path (mergeRulesFile) checks
// rulesMarkersHealthy. The STRIP path (stripRules, via stripRulesFile /
// stripRulesFromProject) removes blocks via an unconditional regex loop. An
// orphan BEGIN marker (no matching END) sitting above a LATER genuine block
// lets rulesBlockRe's non-greedy .*? span from the orphan BEGIN all the way
// to that later block's own END — an in-place splice then deletes everything
// in between, including the developer's own content. On unfixed code this
// test fails with the dev content gone.
func TestReconcileRulesDeEntitlementSkipsMalformedMarker(t *testing.T) {
	claudeDir := t.TempDir()
	proj := t.TempDir()
	if _, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "v1"}}, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(proj, "AGENTS.md")
	genuine := read(t, target)

	orphan := "<!-- ORBEAT-RULES:BEGIN sha=deadbeef0000 x -->\n"
	notes := "# precious dev notes\n\nDo not delete this section.\n"
	corrupted := orphan + notes + genuine
	must(t, os.WriteFile(target, []byte(corrupted), 0o644))

	// De-entitle: the strip pass must now see AGENTS.md's block as undesired.
	res, err := ReconcileRules(claudeDir, []string{proj}, nil, nil)
	if err != nil {
		t.Fatalf("must be non-fatal: %v", err)
	}

	got := read(t, target)
	if got != corrupted {
		t.Fatalf("malformed file must be left untouched:\nwant: %q\ngot:  %q", corrupted, got)
	}
	if !strings.Contains(got, "precious dev notes") {
		t.Fatal("dev notes must survive")
	}
	if len(res.Warnings) == 0 {
		t.Fatalf("expected a malformed-marker warning, got %v", res.Warnings)
	}
	m, err := loadManifest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range m.Rules {
		if filepath.Clean(p) == filepath.Clean(proj) {
			found = true
		}
	}
	if !found {
		t.Fatalf("de-entitled but malformed project must keep its ledger entry for retry; ledger=%v", m.Rules)
	}
}

// Mirror of the above for the `project remove` path (StripProjectRules): a
// malformed marker must block the splice AND keep the ledger entry, rather
// than unconditionally forgetting it the way a normal (healthy) removal does.
func TestStripProjectRulesPreservesLedgerOnMalformedMarker(t *testing.T) {
	claudeDir := t.TempDir()
	proj := t.TempDir()
	if _, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "v1"}}, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(proj, "AGENTS.md")
	genuine := read(t, target)

	orphan := "<!-- ORBEAT-RULES:BEGIN sha=deadbeef0000 x -->\n"
	notes := "# precious dev notes\n\nDo not delete this section.\n"
	corrupted := orphan + notes + genuine
	must(t, os.WriteFile(target, []byte(corrupted), 0o644))

	n, err := StripProjectRules(claudeDir, proj)
	if err != nil {
		t.Fatalf("must be non-fatal: %v", err)
	}
	// The gate is per-file: AGENTS.md's malformed marker must not block
	// CLAUDE.md (healthy, contains only the @AGENTS.md import block) from
	// being stripped normally — so n reflects CLAUDE.md's 1 block, not 0.
	if n != 1 {
		t.Fatalf("expected only CLAUDE.md's healthy block stripped, got n=%d", n)
	}

	got := read(t, target)
	if got != corrupted {
		t.Fatalf("malformed AGENTS.md must be left untouched:\nwant: %q\ngot:  %q", corrupted, got)
	}
	if !strings.Contains(got, "precious dev notes") {
		t.Fatal("dev notes must survive")
	}
	claudeMD := read(t, filepath.Join(proj, "CLAUDE.md"))
	if strings.Contains(claudeMD, "ORBEAT-RULES") {
		t.Fatal("healthy CLAUDE.md should still have been stripped")
	}

	m, err := loadManifest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range m.Rules {
		if filepath.Clean(p) == filepath.Clean(proj) {
			found = true
		}
	}
	if !found {
		t.Fatalf("malformed project must keep its ledger entry after project remove; ledger=%v", m.Rules)
	}
}

func TestReconcileRulesPlanModeWritesNothing(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	arts := []Artifact{{Name: "std", Type: "rule", Content: "RULE-BODY"}}
	if _, err := ReconcileRules(home, []string{proj}, arts, nil); err != nil {
		t.Fatal(err)
	}
	beforeProj, beforeHome := treeSnapshot(t, proj), treeSnapshot(t, home)

	arts[0].Content = "RULE-BODY-CHANGED"
	var p Plan
	res, err := ReconcileRules(home, []string{proj}, arts, &p)
	if err != nil {
		t.Fatalf("plan run must not error: %v", err)
	}
	assertTreeUnchanged(t, "project", proj, beforeProj)
	assertTreeUnchanged(t, "sync root (manifest must not be rewritten either)", home, beforeHome)
	if res.Written != 1 {
		t.Errorf("counter must describe the plan: written=%d, want 1", res.Written)
	}
	// Assert WHICH paths, not just how many: AGENTS.md would change, and the
	// manifest. CLAUDE.md's import already exists from the real run above, so
	// mergeRulesFile leaves it alone and it must NOT appear.
	var sawAgents, sawManifest, sawClaude bool
	for _, c := range p.Changes() {
		switch {
		case strings.HasSuffix(c.Path, manifestName):
			sawManifest = true
		case strings.HasSuffix(c.Path, "AGENTS.md"):
			sawAgents = true
		case strings.HasSuffix(c.Path, "CLAUDE.md"):
			sawClaude = true
		}
	}
	if !sawAgents || !sawManifest || sawClaude {
		t.Errorf("want AGENTS.md + manifest and NOT CLAUDE.md: agents=%v manifest=%v claude=%v changes=%+v",
			sawAgents, sawManifest, sawClaude, p.Changes())
	}
}

func TestReconcileRulesManifestSaveFailureIsRecorded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	claudeDir := t.TempDir()
	proj := t.TempDir()
	if err := os.Chmod(claudeDir, 0o500); err != nil { // manifest save will fail
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(claudeDir, 0o755) })
	res, err := ReconcileRules(claudeDir, []string{proj}, []Artifact{{Type: "rule", Name: "r", Content: "x"}}, nil)

	if err != nil {
		t.Fatalf("manifest-save failure must be non-fatal: %v", err)
	}
	found := false
	for _, f := range res.Failures {
		if strings.Contains(f, "manifest") {
			found = true
		}
	}
	if !found {
		t.Fatalf("manifest-save failure must be recorded in Failures, got %v", res.Failures)
	}
}
