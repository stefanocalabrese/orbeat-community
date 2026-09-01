package syncclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManifestFiles plants a Files ledger with exactly these entries, the way
// a tampered or hand-edited manifest would look on disk.
func writeManifestFiles(t *testing.T, dir string, files ...string) {
	t.Helper()
	data, err := json.Marshal(manifest{Files: files})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readManifestFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	return m.Files
}

// TestReconcileRefusesLedgerEntriesItCouldNeverHaveWritten is A7: the removal
// loop's only guard was resolveContained, which passes anything staying inside
// the sync root, so a manifest naming CLAUDE.md and settings.json deleted both.
// Both are real, load-bearing files: ~/.claude/CLAUDE.md is this client's own
// global-rules target and ~/.claude/settings.json is Claude Code's configuration.
//
// The test asserts three things together, and each one can fail on its own:
// the two hostile entries leave their files alone, a well-shaped entry in the
// SAME run is still removed (so a guard that refuses everything fails here),
// and each refused entry is named in a warning rather than dropped in silence.
func TestReconcileRefusesLedgerEntriesItCouldNeverHaveWritten(t *testing.T) {
	dir := t.TempDir()

	victims := map[string]string{
		"CLAUDE.md":     "my own global instructions",
		"settings.json": `{"model":"opus"}`,
	}
	for rel, content := range victims {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A genuinely managed file, so the run has something legitimate to remove.
	real := filepath.Join(dir, "agents", "rev.md")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("A1"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeManifestFiles(t, dir, "CLAUDE.md", "settings.json", "agents/rev.md")

	res, err := Reconcile(dir, nil, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for rel, content := range victims {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("%s was deleted by a tampered ledger entry: %v", rel, err)
		}
		if string(got) != content {
			t.Fatalf("%s = %q, want it untouched (%q)", rel, got, content)
		}
	}
	if _, err := os.Stat(real); !os.IsNotExist(err) {
		t.Fatalf("agents/rev.md should still have been removed, stat err = %v", err)
	}
	if res.Removed != 1 {
		t.Fatalf("Removed = %d, want 1 (only the well-shaped entry)", res.Removed)
	}
	for rel := range victims {
		if !warningNaming(res.Warnings, rel) {
			t.Fatalf("no warning names %q; warnings = %v", rel, res.Warnings)
		}
	}
	// Dropped, not preserved: the decision is a pure function of the string, so
	// keeping the entry would reprint the warning on every future run and retry
	// a removal that can never happen.
	if got := readManifestFiles(t, dir); len(got) != 0 {
		t.Fatalf("rebuilt ledger = %v, want the refused entries dropped", got)
	}
}

// TestReconcileStillAbortsOnATraversingLedgerEntry pins the ordering of the A7
// shape check against the traversal guard. A `..` entry must stay a fatalError
// that ends the run at exit 2 (the fatalError taxonomy, and the state doctor's
// CheckManifest finding describes); the shape check running first would have
// quietly downgraded a containment escape to a warning.
func TestReconcileStillAbortsOnATraversingLedgerEntry(t *testing.T) {
	dir := t.TempDir()
	writeManifestFiles(t, dir, "../escape.md")

	_, err := Reconcile(dir, nil, nil)
	if err == nil {
		t.Fatal("a traversing ledger entry must abort the run")
	}
	if !isFatal(err) {
		t.Fatalf("err = %v, want a fatalError (exit 2)", err)
	}
}

// TestValidManagedFilePathAcceptsEveryFileBackedType is the anti-drift half of
// A7: the validator derives its accepted set from fileBackedTypes, so adding a
// third file-backed type must not silently make this client refuse to clean up
// files it wrote. It fails the day a type's path function stops being
// reconstructible from one slug-shaped path segment.
func TestValidManagedFilePathAcceptsEveryFileBackedType(t *testing.T) {
	for typ, pathFn := range fileBackedTypes {
		for _, name := range []string{"a", "fmt", "code-review", "x9"} {
			rel := pathFn(name)
			if !validManagedFilePath(rel) {
				t.Fatalf("validManagedFilePath(%q) = false for type %q, so this client can no longer remove a file it wrote", rel, typ)
			}
		}
	}
}

func TestValidManagedFilePathRejects(t *testing.T) {
	for _, bad := range []string{
		"CLAUDE.md",
		"settings.json",
		"AGENTS.md",
		"",
		"skills/fmt/README.md",
		"skills/fmt",
		"skills/Fmt/SKILL.md",
		"skills/../SKILL.md",
		"agents/rev.txt",
		"agents/rev",
		"agents/sub/rev.md",
		"plugins/rev.md",
		"skills/fmt/SKILL.md/extra",
		"SKILL.md",
	} {
		if validManagedFilePath(bad) {
			t.Fatalf("validManagedFilePath(%q) = true, want false", bad)
		}
	}
}

// TestReconcileAdoptsAnIdenticalUnledgeredFile is A9: doctor tells an operator
// to "delete the manifest entirely and run 'orbeat-sync sync'", and before this
// fix that produced an empty ledger and froze every rendered artifact at its
// current content, on that run and on every run after, reported only as
// skipped.
//
// The load-bearing assertion is the THIRD run, not the second. A run that
// merely reports Unchanged proves nothing about ownership: the proof that the
// ledger was really rebuilt is that de-entitling the artifacts afterwards
// actually removes the files.
func TestReconcileAdoptsAnIdenticalUnledgeredFile(t *testing.T) {
	dir := t.TempDir()
	arts := []Artifact{
		{ID: "id-fmt", Revision: 3, Type: "skill", Name: "fmt", Content: "S1"},
		{ID: "id-rev", Revision: 4, Type: "subagent", Name: "rev", Content: "A1"},
	}
	if _, err := Reconcile(dir, arts, nil); err != nil {
		t.Fatalf("reconcile1: %v", err)
	}

	// The remedy, exactly as doctor words it.
	if err := os.Remove(filepath.Join(dir, manifestName)); err != nil {
		t.Fatal(err)
	}

	res, err := Reconcile(dir, arts, nil)
	if err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("Skipped = %v, want the identical files adopted, not skipped", res.Skipped)
	}
	if res.Unchanged != 2 || res.Added != 0 || res.Updated != 0 {
		t.Fatalf("res = %+v, want 2 unchanged and no writes", res)
	}
	if len(res.Applied) != 2 {
		t.Fatalf("Applied = %+v, want both artifacts", res.Applied)
	}
	files := readManifestFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("rebuilt ledger = %v, want both rendered paths", files)
	}

	// Third run: de-entitle everything. Only a genuinely rebuilt ledger can
	// remove them.
	res3, err := Reconcile(dir, nil, nil)
	if err != nil {
		t.Fatalf("reconcile3: %v", err)
	}
	if res3.Removed != 2 {
		t.Fatalf("Removed = %d, want 2: the ledger was not really rebuilt", res3.Removed)
	}
	for _, rel := range []string{"skills/fmt/SKILL.md", "agents/rev.md"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Fatalf("%s still on disk after de-entitlement, stat err = %v", rel, err)
		}
	}
}

// TestReconcileDoesNotAdoptAFileWhoseContentDiffers is the other half of A9,
// and the reason the fix is not broader: adopting on presence alone would take
// ownership of a file the developer may have written by hand, and the next
// de-entitlement would then delete it. TestReconcileNeverTouchesUnmanaged
// covers the same refusal for a first-ever sync; this one covers the
// deleted-manifest path the remedy actually sends people down.
func TestReconcileDoesNotAdoptAFileWhoseContentDiffers(t *testing.T) {
	dir := t.TempDir()
	arts := []Artifact{{ID: "id-fmt", Revision: 1, Type: "skill", Name: "fmt", Content: "SERVER"}}
	if _, err := Reconcile(dir, arts, nil); err != nil {
		t.Fatalf("reconcile1: %v", err)
	}
	rel := filepath.Join("skills", "fmt", "SKILL.md")
	if err := os.WriteFile(filepath.Join(dir, rel), []byte("MINE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, manifestName)); err != nil {
		t.Fatal(err)
	}

	res, err := Reconcile(dir, arts, nil)
	if err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "skills/fmt/SKILL.md" {
		t.Fatalf("Skipped = %v, want the differing file skipped", res.Skipped)
	}
	if read(t, filepath.Join(dir, rel)) != "MINE" {
		t.Fatal("reconcile clobbered a file whose content differed")
	}
	if got := readManifestFiles(t, dir); len(got) != 0 {
		t.Fatalf("ledger = %v, want the differing file NOT adopted", got)
	}
}

func warningNaming(warnings []string, needle string) bool {
	for _, w := range warnings {
		if strings.Contains(w, needle) {
			return true
		}
	}
	return false
}
