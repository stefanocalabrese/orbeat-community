package syncclient

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestReconcileAddUpdateRemove(t *testing.T) {
	dir := t.TempDir()

	// First sync: one skill + one subagent.
	r1, err := Reconcile(dir, []Artifact{
		{Type: "skill", Name: "fmt", Content: "S1"},
		{Type: "subagent", Name: "rev", Content: "A1"},
	}, nil)
	if err != nil {
		t.Fatalf("reconcile1: %v", err)
	}
	if r1.Added != 2 || r1.Updated != 0 || r1.Removed != 0 {
		t.Fatalf("r1 = %+v, want add 2", r1)
	}
	if read(t, filepath.Join(dir, "skills", "fmt", "SKILL.md")) != "S1" {
		t.Fatal("skill content wrong")
	}
	if read(t, filepath.Join(dir, "agents", "rev.md")) != "A1" {
		t.Fatal("agent content wrong")
	}

	// Second sync: fmt updated, rev dropped, new skill added.
	r2, err := Reconcile(dir, []Artifact{
		{Type: "skill", Name: "fmt", Content: "S2"},
		{Type: "skill", Name: "lint", Content: "S3"},
	}, nil)
	if err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	if r2.Added != 1 || r2.Updated != 1 || r2.Removed != 1 {
		t.Fatalf("r2 = %+v, want add1 upd1 rem1", r2)
	}
	if read(t, filepath.Join(dir, "skills", "fmt", "SKILL.md")) != "S2" {
		t.Fatal("fmt not updated")
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "rev.md")); !os.IsNotExist(err) {
		t.Fatal("rev should have been removed")
	}
}

// S7: a steady-state second sync (identical artifact set + content) must not
// rewrite anything: 0 added/updated, everything counted Unchanged, and the
// managed files' mtimes untouched (a rewrite would bump them, retriggering
// file-watchers and firing the "restart Claude Code" hint every run).
func TestReconcileSteadyStateUnchanged(t *testing.T) {
	dir := t.TempDir()
	arts := []Artifact{
		{Type: "skill", Name: "fmt", Content: "S1"},
		{Type: "subagent", Name: "rev", Content: "A1"},
	}
	if _, err := Reconcile(dir, arts, nil); err != nil {
		t.Fatalf("reconcile1: %v", err)
	}

	// Pin mtimes to a recognizable past instant so any rewrite is detectable
	// regardless of filesystem timestamp granularity.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	paths := []string{
		filepath.Join(dir, "skills", "fmt", "SKILL.md"),
		filepath.Join(dir, "agents", "rev.md"),
	}
	for _, p := range paths {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	r2, err := Reconcile(dir, arts, nil)
	if err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	if r2.Added != 0 || r2.Updated != 0 || r2.Removed != 0 || r2.Unchanged != 2 {
		t.Fatalf("steady state = %+v, want 0 added/updated/removed, 2 unchanged", r2)
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if !info.ModTime().Equal(past) {
			t.Fatalf("%s was rewritten in steady state (mtime %v, want %v)", p, info.ModTime(), past)
		}
	}
}

// S7: a genuinely changed artifact must still be written (and counted Updated)
// when it sits alongside unchanged ones.
func TestReconcileChangedAmongUnchanged(t *testing.T) {
	dir := t.TempDir()
	if _, err := Reconcile(dir, []Artifact{
		{Type: "skill", Name: "fmt", Content: "S1"},
		{Type: "skill", Name: "lint", Content: "L1"},
	}, nil); err != nil {
		t.Fatalf("reconcile1: %v", err)
	}
	r2, err := Reconcile(dir, []Artifact{
		{Type: "skill", Name: "fmt", Content: "S2"},  // changed
		{Type: "skill", Name: "lint", Content: "L1"}, // unchanged
	}, nil)
	if err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	if r2.Updated != 1 || r2.Unchanged != 1 {
		t.Fatalf("r2 = %+v, want 1 updated + 1 unchanged", r2)
	}
	if read(t, filepath.Join(dir, "skills", "fmt", "SKILL.md")) != "S2" {
		t.Fatal("changed skill not rewritten")
	}
}

// S7: artifact writes go through writeFileAtomic — a successful run leaves no
// temp litter next to the artifact files (temp+rename, never O_TRUNC in place).
func TestReconcileArtifactWriteLeavesNoTempLitter(t *testing.T) {
	dir := t.TempDir()
	if _, err := Reconcile(dir, []Artifact{{Type: "skill", Name: "fmt", Content: "S1"}}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	litter, err := filepath.Glob(filepath.Join(dir, "skills", "fmt", "SKILL.md.tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(litter) != 0 {
		t.Fatalf("temp litter left behind: %v", litter)
	}
}

func TestReconcileNeverTouchesUnmanaged(t *testing.T) {
	dir := t.TempDir()
	// A hand-authored personal skill with the SAME name an artifact will use.
	mine := filepath.Join(dir, "skills", "fmt", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("HAND-MADE"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Reconcile(dir, []Artifact{{Type: "skill", Name: "fmt", Content: "FROM-ORBEAT"}}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(r.Skipped) != 1 || r.Skipped[0] != "skills/fmt/SKILL.md" {
		t.Fatalf("expected the unmanaged collision to be skipped, got %+v", r.Skipped)
	}
	if read(t, mine) != "HAND-MADE" {
		t.Fatal("reconcile clobbered a hand-authored file")
	}
}

func TestReconcileRejectsUnsafeName(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../../evil", "..", "a/b", "", "Up", "with.dot"} {
		if _, err := Reconcile(dir, []Artifact{{Type: "skill", Name: bad, Content: "X"}}, nil); err == nil {
			t.Fatalf("expected error for unsafe name %q", bad)
		}
	}
	// Confirm nothing was written outside the sync root.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
		t.Fatal("reconcile wrote outside claudeDir")
	}
}

// The remove path must reject a tampered manifest whose entry escapes the sync root,
// and must NOT delete the out-of-root file.
func TestReconcileRejectsTamperedManifest(t *testing.T) {
	dir := t.TempDir()
	evil := filepath.Join(filepath.Dir(dir), "evil-target.md")
	if err := os.WriteFile(evil, []byte("DO NOT DELETE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".orbeat-sync-manifest.json"),
		[]byte(`{"files":["../evil-target.md"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(dir, nil, nil); err == nil {
		t.Fatal("expected error: a manifest entry escaping the sync root must be rejected")
	}
	if _, err := os.Stat(evil); err != nil {
		t.Fatalf("reconcile deleted a file outside the sync root: %v", err)
	}
}

// tempManifestLitter returns any leftover atomic-write temp files in dir.
func tempManifestLitter(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, manifestName+".tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return m
}

// The manifest is written atomically (temp + rename), so a successful run must
// leave no temp litter behind and the manifest must round-trip through loadManifest.
func TestReconcileManifestWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if _, err := Reconcile(dir, []Artifact{
		{Type: "skill", Name: "fmt", Content: "S1"},
		{Type: "subagent", Name: "rev", Content: "A1"},
	}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if litter := tempManifestLitter(t, dir); len(litter) != 0 {
		t.Fatalf("atomic write left temp litter: %v", litter)
	}
	// The renamed manifest must carry the 0644 we Chmod the temp to (CreateTemp makes 0600).
	fi, err := os.Stat(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Fatalf("manifest mode = %o, want 0644", perm)
	}
	got, err := loadManifest(dir)
	if err != nil {
		t.Fatalf("loadManifest: %v", err) // a torn/truncated manifest would fail to parse here
	}
	want := []string{"agents/rev.md", "skills/fmt/SKILL.md"}
	if len(got.Files) != len(want) || got.Files[0] != want[0] || got.Files[1] != want[1] {
		t.Fatalf("manifest = %v, want %v", got.Files, want)
	}
}

// A stale temp file (e.g. from a crash before the rename) must not break a later
// run, and is self-healed: the next save removes orphaned .tmp-* files so they
// don't accumulate in the user's home.
func TestReconcileSelfHealsStaleTempLitter(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, manifestName+".tmp-stale")
	if err := os.WriteFile(stale, []byte("{partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Reconcile(dir, []Artifact{{Type: "skill", Name: "fmt", Content: "S1"}}, nil)
	if err != nil {
		t.Fatalf("reconcile with stale temp present: %v", err)
	}
	if r.Added != 1 {
		t.Fatalf("r = %+v, want add 1", r)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp should have been self-healed away, stat err = %v", err)
	}
	if litter := tempManifestLitter(t, dir); len(litter) != 0 {
		t.Fatalf("temp litter remains after run: %v", litter)
	}
	got, err := loadManifest(dir)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0] != "skills/fmt/SKILL.md" {
		t.Fatalf("manifest = %v, want [skills/fmt/SKILL.md]", got.Files)
	}
}

// Containment must reject a sibling dir that shares a string prefix with the root
// (e.g. .claude vs .claude-evil) — the classic HasPrefix-confusion bug class.
func TestResolveContainedRejectsSiblingPrefix(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude")
	if _, err := resolveContained(dir, "../.claude-evil/x"); err == nil {
		t.Fatal("sibling dir sharing a prefix must be rejected")
	}
	if _, err := resolveContained(dir, "skills/fmt/SKILL.md"); err != nil {
		t.Fatalf("legitimate path wrongly rejected: %v", err)
	}
}

func TestReconcilePreservesSeedsLedger(t *testing.T) {
	dir := t.TempDir()
	// Simulate a manifest written by a prior seed pass.
	m := manifest{
		Files: []string{"skills/old/SKILL.md"},
		Seeds: map[string][]string{"rev": {filepath.Join(dir, "agent-memory", "rev", "MEMORY.md")}},
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills", "old"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "old", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := saveManifest(dir, m, nil); err != nil {
		t.Fatalf("save: %v", err)
	}

	// An artifact pass must rewrite Files without touching Seeds.
	if _, err := Reconcile(dir, []Artifact{{Type: "skill", Name: "fresh", Content: "c"}}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := loadManifest(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0] != "skills/fresh/SKILL.md" {
		t.Fatalf("files=%v", got.Files)
	}
	if len(got.Seeds["rev"]) != 1 {
		t.Fatalf("seeds ledger lost: %+v", got.Seeds)
	}
}

// TestReconcilePreservesRulesLedger mirrors TestReconcilePreservesSeedsLedger
// for the Rules ledger. This path went production-live only with the rule-type
// fix: before it, Reconcile hard-errored on any artifact set containing a
// "rule", so a manifest carrying both a populated Rules ledger AND a
// successful Reconcile run over a mixed skill+rule artifact set had never
// executed. Per rules.go, the Rules ledger is the ONLY record of which project
// roots carry an ORBEAT-RULES block — "a lost manifest cannot be reconstructed
// from projects alone" — so a future refactor silently dropping it would
// orphan every managed block in every dev repo with no recovery path.
func TestReconcilePreservesRulesLedger(t *testing.T) {
	dir := t.TempDir()
	// Simulate a manifest written by a prior rules pass.
	m := manifest{
		Files: []string{"skills/old/SKILL.md"},
		Rules: []string{filepath.Join(dir, "proj-a"), filepath.Join(dir, "proj-b")},
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills", "old"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "old", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := saveManifest(dir, m, nil); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A mixed skill+rule Reconcile run must rewrite Files, skip the rule
	// (owned by ReconcileRules), and leave Rules untouched.
	if _, err := Reconcile(dir, []Artifact{
		{Type: "skill", Name: "fresh", Content: "c"},
		{Type: "rule", Name: "coding-standards", Content: "# Coding standards"},
	}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := loadManifest(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0] != "skills/fresh/SKILL.md" {
		t.Fatalf("files=%v", got.Files)
	}
	wantRules := []string{filepath.Join(dir, "proj-a"), filepath.Join(dir, "proj-b")}
	if len(got.Rules) != len(wantRules) {
		t.Fatalf("rules ledger lost: got %+v, want %+v", got.Rules, wantRules)
	}
	for i, r := range wantRules {
		if got.Rules[i] != r {
			t.Fatalf("rules ledger corrupted: got %+v, want %+v", got.Rules, wantRules)
		}
	}
}

// TestReconcileSkipsRuleType is the production-regression case: a mix of a
// skill and a rule must NOT error out. The skill is file-backed and must be
// written; the rule is owned by ReconcileRules and must be silently skipped
// here (no file written for it, no warning either).
func TestReconcileSkipsRuleType(t *testing.T) {
	dir := t.TempDir()
	r, err := Reconcile(dir, []Artifact{
		{Type: "skill", Name: "fmt", Content: "S1"},
		{Type: "rule", Name: "coding-standards", Content: "# Coding standards"},
	}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if read(t, filepath.Join(dir, "skills", "fmt", "SKILL.md")) != "S1" {
		t.Fatal("skill should have been written")
	}
	if _, err := os.Stat(filepath.Join(dir, "rules")); !os.IsNotExist(err) {
		t.Fatal("reconcile must not write anything for a rule artifact")
	}
	if len(r.Warnings) != 0 {
		t.Fatalf("skipping a rule must not warn, got %v", r.Warnings)
	}
	if r.Added != 1 {
		t.Fatalf("r = %+v, want add 1 (only the skill)", r)
	}
}

// TestReconcileOnlyRuleWritesNothing covers a sync payload made entirely of
// rule artifacts: no error, nothing written, no warning.
func TestReconcileOnlyRuleWritesNothing(t *testing.T) {
	dir := t.TempDir()
	r, err := Reconcile(dir, []Artifact{
		{Type: "rule", Name: "coding-standards", Content: "# Coding standards"},
	}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if r.Added != 0 || r.Updated != 0 || r.Removed != 0 {
		t.Fatalf("r = %+v, want all-zero", r)
	}
	if len(r.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", r.Warnings)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == manifestName {
			continue
		}
		t.Fatalf("unexpected entry written for a rule-only sync: %s", e.Name())
	}
}

// TestReconcileWarnsOnUnknownTypeButKeepsGoing is the forward-compat case: an
// unrecognized artifact type (e.g. a future server-side type this client
// doesn't understand yet) must produce a warning and NEVER abort the sync —
// the skill alongside it must still be delivered.
func TestReconcileWarnsOnUnknownTypeButKeepsGoing(t *testing.T) {
	dir := t.TempDir()
	r, err := Reconcile(dir, []Artifact{
		{Type: "skill", Name: "fmt", Content: "S1"},
		{Type: "plugin", Name: "future-thing", Content: "???"},
	}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if read(t, filepath.Join(dir, "skills", "fmt", "SKILL.md")) != "S1" {
		t.Fatal("skill should still have been written despite the unknown type alongside it")
	}
	if len(r.Warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %v", r.Warnings)
	}
	if !strings.Contains(r.Warnings[0], "plugin") {
		t.Fatalf("warning should mention the unknown type, got %q", r.Warnings[0])
	}
	if r.Added != 1 {
		t.Fatalf("r = %+v, want add 1 (only the skill)", r)
	}
}

// A non-fatal write failure on one artifact is recorded; the others still land.
func TestReconcileWriteFailureIsolated(t *testing.T) {
	dir := t.TempDir()
	// A regular file at skills/bad makes os.MkdirAll("skills/bad") fail ENOTDIR,
	// so the "bad" artifact's write fails while "good" writes normally. (Root-safe.)
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "bad"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Reconcile(dir, []Artifact{
		{Type: "skill", Name: "bad", Content: "X"},
		{Type: "skill", Name: "good", Content: "G"},
	}, nil)
	if err != nil {
		t.Fatalf("must not return a fatal error for a per-artifact I/O failure: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("want 1 failure, got %d (%v)", len(res.Failures), res.Failures)
	}
	if read(t, filepath.Join(dir, "skills", "good", "SKILL.md")) != "G" {
		t.Fatal("the healthy skill must still be written")
	}
}

// A non-fatal UPDATE-write failure keeps the file's ledger entry so a later run
// retries rather than orphaning a possibly-truncated file.
func TestReconcileWriteFailurePreservesLedger(t *testing.T) {
	dir := t.TempDir()
	// Run 1: establish "fmt" as a managed file.
	if _, err := Reconcile(dir, []Artifact{{Type: "skill", Name: "fmt", Content: "V1"}}, nil); err != nil {
		t.Fatal(err)
	}
	// Replace the managed file with a directory so run 2's update write fails (EISDIR).
	p := filepath.Join(dir, "skills", "fmt", "SKILL.md")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Reconcile(dir, []Artifact{{Type: "skill", Name: "fmt", Content: "V2"}}, nil)
	if err != nil {
		t.Fatalf("update-write I/O failure must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("want 1 failure, got %v", res.Failures)
	}
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range m.Files {
		if f == "skills/fmt/SKILL.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("write-failed managed file must stay in the ledger for retry; ledger=%v", m.Files)
	}
}

// A remove that fails non-fatally keeps the file's ledger entry so a later run retries.
func TestReconcileRemoveFailurePreservesLedger(t *testing.T) {
	dir := t.TempDir()
	// Ledger says we manage skills/gone/SKILL.md, but on disk it is a NON-EMPTY
	// directory, so os.Remove fails with ENOTEMPTY (root-safe).
	inner := filepath.Join(dir, "skills", "gone", "SKILL.md")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName),
		[]byte(`{"files":["skills/gone/SKILL.md"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// No artifacts -> "gone" is de-entitled -> remove pass tries and fails.
	res, err := Reconcile(dir, nil, nil)
	if err != nil {
		t.Fatalf("remove I/O failure must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 || res.Removed != 0 {
		t.Fatalf("want 1 failure + 0 removed, got failures=%v removed=%d", res.Failures, res.Removed)
	}
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range m.Files {
		if f == "skills/gone/SKILL.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("remove-failed file must stay in the ledger for retry; ledger=%v", m.Files)
	}
}

// A corrupt manifest is fatal.
func TestReconcileCorruptManifestIsFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Reconcile(dir, []Artifact{{Type: "skill", Name: "x", Content: "c"}}, nil)
	if err == nil || !isFatal(err) {
		t.Fatalf("corrupt manifest must be fatal, got %v", err)
	}
}

func TestLoadManifestBackCompat(t *testing.T) {
	dir := t.TempDir()
	// A Slice-A manifest has only "files" — must load with a nil Seeds map.
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(`{"files":["agents/a.md"]}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := loadManifest(dir)
	if err != nil || len(m.Files) != 1 || m.Seeds != nil {
		t.Fatalf("m=%+v err=%v", m, err)
	}
}

func TestReconcilePlanModeWritesNothingButCounts(t *testing.T) {
	dir := t.TempDir()
	arts := []Artifact{{Name: "alpha", Type: "subagent", Content: "BODY-A"}}

	// A real sync first, so there is a manifest and one managed file.
	if _, err := Reconcile(dir, arts, nil); err != nil {
		t.Fatal(err)
	}
	before := treeSnapshot(t, dir)

	// Now plan a run that would add one file and update another.
	arts[0].Content = "BODY-A-CHANGED"
	arts = append(arts, Artifact{Name: "beta", Type: "subagent", Content: "BODY-B"})
	var p Plan
	res, err := Reconcile(dir, arts, &p)
	if err != nil {
		t.Fatalf("plan run must not error: %v", err)
	}
	assertTreeUnchanged(t, dir, dir, before)
	if res.Added != 1 || res.Updated != 1 {
		t.Errorf("counters must describe the plan: added=%d updated=%d, want 1/1", res.Added, res.Updated)
	}
	// Three, not two: the two artifact writes plus the manifest. The ledger is
	// recorded because the Plan is the complete set of intended mutations — a
	// plan that silently omitted one would misrepresent what the code does, and
	// the saveManifest red-proof below depends on it being covered. The manifest
	// is filtered out of the USER-FACING report in cmd/sync, not here.
	changes := p.Changes()
	if len(changes) != 3 {
		t.Fatalf("want 3 recorded changes (2 artifacts + manifest), got %+v", changes)
	}
	// A bare count doesn't prove WHICH three: it would pass just as well if the
	// manifest were recorded twice and an artifact not at all. Pin the actual set.
	wantSuffixes := map[string]bool{
		filepath.Join("agents", "alpha.md"): false,
		filepath.Join("agents", "beta.md"):  false,
		manifestName:                        false,
	}
	for _, c := range changes {
		matched := ""
		for suffix := range wantSuffixes {
			if strings.HasSuffix(c.Path, suffix) {
				matched = suffix
				break
			}
		}
		if matched == "" {
			t.Fatalf("unexpected recorded change path %q", c.Path)
		}
		if wantSuffixes[matched] {
			t.Fatalf("path suffix %q recorded more than once: %+v", matched, changes)
		}
		wantSuffixes[matched] = true
	}
	for suffix, seen := range wantSuffixes {
		if !seen {
			t.Fatalf("expected a recorded change ending in %q, got %+v", suffix, changes)
		}
	}
}

// TestReconcilePlanModeAgainstAbsentRoot is B1: a plan against a sync root
// that has never existed must report every entitled artifact as a create,
// record no failures, and — like every other plan-mode run — write nothing,
// including not creating the sync root directory itself. An earlier version
// of Reconcile special-cased an absent claudeDir in plan mode by returning
// the zero result (0 added, 0 handled), which is what a first-ever
// `sync --dry-run` — plausibly the commonest invocation of this whole
// feature — would have hit.
func TestReconcilePlanModeAgainstAbsentRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-synced")
	arts := []Artifact{
		{Type: "skill", Name: "s1", Content: "S1"},
		{Type: "subagent", Name: "a1", Content: "A1"},
	}

	var p Plan
	res, err := Reconcile(dir, arts, &p)
	if err != nil {
		t.Fatalf("plan run against an absent root must not error: %v", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("plan mode must not create the sync root, stat err=%v", statErr)
	}
	if res.Added != 2 || res.Handled != 2 {
		t.Fatalf("every entitled artifact must plan as a create: res=%+v", res)
	}
	if res.Updated != 0 || res.Removed != 0 || res.Unchanged != 0 {
		t.Fatalf("a first-ever plan has nothing to update/remove/leave unchanged: res=%+v", res)
	}
	if len(res.Failures) != 0 {
		t.Fatalf("a first-ever plan must record no failures, got %v", res.Failures)
	}

	changes := p.Changes()
	if len(changes) != 3 { // two artifacts + the manifest bookkeeping entry
		t.Fatalf("want 3 recorded changes (2 artifacts + manifest), got %+v", changes)
	}
	for _, c := range changes {
		if c.Op != OpCreate {
			t.Errorf("change %+v: want op=create against a never-synced root", c)
		}
	}
}

// treeDigest hashes every path under root, DIRECTORIES INCLUDED. Directories
// are the point: three os.MkdirAll calls bypass the rooted seam, and a
// files-only digest cannot see a dry run that creates directory structure.
//
// Deliberately content-and-structure only (relpath | isDir | content) — NOT
// permissions or mtime. Some callers (plan_apply_test.go's "setup runs
// diverged" checks) compare this digest across two INDEPENDENTLY created
// fixture trees that are only guaranteed to agree on content, never on
// wall-clock mtimes; folding mtime in here would make those checks flaky for
// a reason that has nothing to do with what they test. The writes-nothing
// gates that need permissions/mtime sensitivity — same tree, before vs after
// an operation that must perform zero I/O — use treeSnapshot/
// assertTreeUnchanged below instead, which also names the exact property
// that moved rather than just reporting a changed hash.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(h, "%s|%v|", rel, d.IsDir())
		if !d.IsDir() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// entrySnapshot is one path's on-disk state, as seen by treeSnapshot: kind,
// full os.Lstat mode (permission bits AND type bits, e.g. a regular file
// silently becoming a symlink), mtime, and content. Metadata comes from
// os.Lstat rather than os.Stat: this package never needs to compare what a
// symlink resolves to (that target can change for reasons entirely outside
// this tree, or point nowhere), only whether the symlink ENTRY stored under
// root — the thing this code actually wrote or read — was itself disturbed.
// Content is still read via os.ReadFile, matching treeDigest, so a symlink's
// pointed-to content is still covered by the content field even though its
// mode/mtime are the link's own.
type entrySnapshot struct {
	isDir   bool
	mode    fs.FileMode
	mtime   time.Time
	content string // meaningless (left zero) for directories
}

// treeSnapshot captures entrySnapshot for every path under root, keyed by
// root-relative path.
func treeSnapshot(t *testing.T, root string) map[string]entrySnapshot {
	t.Helper()
	out := map[string]entrySnapshot{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		fi, statErr := os.Lstat(p)
		if statErr != nil {
			return statErr
		}
		e := entrySnapshot{isDir: d.IsDir(), mode: fi.Mode(), mtime: fi.ModTime()}
		if !d.IsDir() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			e.content = string(b)
		}
		out[rel] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertTreeUnchanged is the writes-nothing gate: it re-snapshots root and
// fails naming exactly which path and which property (existence, kind, mode,
// mtime, or content) differs from before, rather than reporting only that
// some opaque hash no longer matches. label appears in the failure message —
// callers typically pass the root path itself, or a more descriptive name
// when comparing several labeled roots (see assertAllTreesUnchanged).
func assertTreeUnchanged(t *testing.T, label string, root string, before map[string]entrySnapshot) {
	t.Helper()
	after := treeSnapshot(t, root)
	for rel, b := range before {
		a, ok := after[rel]
		if !ok {
			t.Errorf("%s: %q vanished", label, rel)
			continue
		}
		if a.isDir != b.isDir {
			t.Errorf("%s: %q changed kind: dir=%v -> dir=%v", label, rel, b.isDir, a.isDir)
		}
		if a.mode != b.mode {
			t.Errorf("%s: %q mode changed: %v -> %v", label, rel, b.mode, a.mode)
		}
		if !a.mtime.Equal(b.mtime) {
			t.Errorf("%s: %q mtime changed: %v -> %v", label, rel, b.mtime, a.mtime)
		}
		if a.content != b.content {
			t.Errorf("%s: %q content changed", label, rel)
		}
	}
	for rel := range after {
		if _, existed := before[rel]; !existed {
			t.Errorf("%s: %q appeared", label, rel)
		}
	}
}
