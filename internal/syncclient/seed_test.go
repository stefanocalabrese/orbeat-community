package syncclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "deep", "MEMORY.md")

	// Creates parent dirs and the file with the requested perms.
	if err := writeFileAtomic(target, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "v1" {
		t.Fatalf("read back: %v %q", err, data)
	}
	if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("file perms: %v %v", err, st.Mode())
	}

	// Overwrite; also plant an orphaned temp from a "crashed" prior run — it must be cleaned.
	orphan := filepath.Join(dir, "deep", "MEMORY.md.tmp-orphan")
	if err := os.WriteFile(orphan, []byte("junk"), 0o644); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}
	if err := writeFileAtomic(target, []byte("v2"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "v2" {
		t.Fatalf("want v2, got %q", data)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphaned temp not cleaned")
	}
	// No temp litter after a successful run.
	if stale, _ := filepath.Glob(filepath.Join(dir, "deep", "MEMORY.md.tmp-*")); len(stale) != 0 {
		t.Fatalf("temp litter: %v", stale)
	}
}

func TestMergeSeed(t *testing.T) {
	body := "## Standards\nseed body"
	block := renderSeedBlock("rev", body)

	t.Run("into empty content", func(t *testing.T) {
		out, changed, _ := mergeSeed("", "rev", body)
		if !changed || out != block {
			t.Fatalf("changed=%v out=%q", changed, out)
		}
	})

	t.Run("prepends above existing notes with one blank line", func(t *testing.T) {
		out, changed, _ := mergeSeed("- my own note\n", "rev", body)
		want := block + "\n- my own note\n"
		if !changed || out != want {
			t.Fatalf("changed=%v\nout:  %q\nwant: %q", changed, out, want)
		}
	})

	t.Run("idempotent no-op when hash unchanged", func(t *testing.T) {
		existing := block + "\n- my own note\n"
		out, changed, _ := mergeSeed(existing, "rev", body)
		if changed || out != existing {
			t.Fatalf("expected no-op, changed=%v", changed)
		}
	})

	t.Run("replaces block on new body, preserving notes", func(t *testing.T) {
		existing := block + "\n- my own note\n"
		out, changed, _ := mergeSeed(existing, "rev", "new seed v2")
		want := renderSeedBlock("rev", "new seed v2") + "\n- my own note\n"
		if !changed || out != want {
			t.Fatalf("changed=%v\nout:  %q\nwant: %q", changed, out, want)
		}
	})

	t.Run("re-hoists a relocated block to the top on change", func(t *testing.T) {
		existing := "- note above\n\n" + block + "\n- note below\n"
		out, changed, _ := mergeSeed(existing, "rev", "new seed v2")
		want := renderSeedBlock("rev", "new seed v2") + "\n- note above\n\n- note below\n"
		if !changed || out != want {
			t.Fatalf("changed=%v\nout:  %q\nwant: %q", changed, out, want)
		}
	})

	t.Run("relocated but unchanged block stays put", func(t *testing.T) {
		existing := "- note above\n\n" + block
		out, changed, _ := mergeSeed(existing, "rev", body)
		if changed || out != existing {
			t.Fatalf("expected no-op for unchanged relocated block, changed=%v", changed)
		}
	})

	// Edge seam: a mid-file block with no trailing newline of its own (the
	// gap between it and the next line is contributed entirely by that next
	// line's leading "\n") must collapse the same way as the normal case —
	// the fix must not depend on the block itself carrying the newline.
	t.Run("re-hoist still collapses the gap when the old block had no trailing newline", func(t *testing.T) {
		noTrailingNL := strings.TrimSuffix(block, "\n")
		existing := "- note above\n\n" + noTrailingNL + "\n- note below\n"
		out, changed, _ := mergeSeed(existing, "rev", "new seed v2")
		want := renderSeedBlock("rev", "new seed v2") + "\n- note above\n\n- note below\n"
		if !changed || out != want {
			t.Fatalf("changed=%v\nout:  %q\nwant: %q", changed, out, want)
		}
	})

	// Edge seam: a body containing a literal "-->" must not be mistaken for
	// the end sentinel — only the exact "ORBEAT-SEED:END <name> -->" text
	// closes the block. Verified via round-trip idempotency rather than a
	// hand-computed byte string.
	t.Run("body containing a literal --> is not mistaken for the end marker", func(t *testing.T) {
		trapBody := "line one\n--> not an end marker\nline two"
		out, changed, _ := mergeSeed("", "rev", trapBody)
		if !changed {
			t.Fatalf("expected change")
		}
		if got := strings.Count(out, "ORBEAT-SEED:BEGIN rev "); got != 1 {
			t.Fatalf("BEGIN count = %d, out=%q", got, out)
		}
		if got := strings.Count(out, "ORBEAT-SEED:END rev "); got != 1 {
			t.Fatalf("END count = %d, out=%q", got, out)
		}
		out2, changed2, _ := mergeSeed(out, "rev", trapBody)
		if changed2 || out2 != out {
			t.Fatalf("expected stable idempotent merge, changed=%v", changed2)
		}
	})

	t.Run("name is not a prefix trap", func(t *testing.T) {
		// A block for "rev" must not be matched when merging "rev-two".
		existing := block + "\n- note\n"
		out, changed, _ := mergeSeed(existing, "rev-two", "other seed")
		if !changed || !strings.Contains(out, "ORBEAT-SEED:BEGIN rev ") || !strings.Contains(out, "ORBEAT-SEED:BEGIN rev-two ") {
			t.Fatalf("prefix collision:\n%s", out)
		}
	})

	// False-no-op probe: the old heuristic (substring search for
	// " sha=<newhash> " anywhere in the matched block) could be fooled by a
	// body whose own text happens to contain that exact token. The fix
	// compares against the hash captured from the BEGIN marker specifically,
	// so this must still register as a real change.
	t.Run("a body containing a fake sha token does not fool the hash compare", func(t *testing.T) {
		newBody := "new content"
		fakeHash := seedHash(newBody)
		oldBody := "decoy sha=" + fakeHash + " embedded"
		existing := renderSeedBlock("rev", oldBody)
		out, changed, _ := mergeSeed(existing, "rev", newBody)
		want := renderSeedBlock("rev", newBody)
		if !changed || out != want {
			t.Fatalf("changed=%v\nout:  %q\nwant: %q", changed, out, want)
		}
	})

	// The hash follows the written (trimmed) form, so a body differing only
	// in trailing newlines from what's already on disk must be a no-op.
	t.Run("trailing-newline-only difference in body is a no-op", func(t *testing.T) {
		existing := block
		out, changed, _ := mergeSeed(existing, "rev", body+"\n\n\n")
		if changed || out != existing {
			t.Fatalf("expected no-op for trailing-newline-only body difference, changed=%v out=%q", changed, out)
		}
	})
}

func TestStripSeed(t *testing.T) {
	block := renderSeedBlock("rev", "seed body")

	t.Run("strips block, keeps notes", func(t *testing.T) {
		out, changed := stripSeed(block+"\n- my own note\n", "rev")
		if !changed || out != "- my own note\n" {
			t.Fatalf("changed=%v out=%q", changed, out)
		}
	})

	t.Run("no-op when absent", func(t *testing.T) {
		out, changed := stripSeed("- just notes\n", "rev")
		if changed || out != "- just notes\n" {
			t.Fatalf("changed=%v out=%q", changed, out)
		}
	})

	t.Run("block-only file becomes empty, not deleted semantics", func(t *testing.T) {
		out, changed := stripSeed(block, "rev")
		if !changed || out != "" {
			t.Fatalf("changed=%v out=%q", changed, out)
		}
	})

	// Edge seam: a file that ends mid-marker line (no trailing newline after
	// the END sentinel) must still be fully stripped, not left with a
	// dangling partial marker.
	t.Run("strips a block with no trailing newline", func(t *testing.T) {
		out, changed := stripSeed(strings.TrimSuffix(block, "\n"), "rev")
		if !changed || out != "" {
			t.Fatalf("changed=%v out=%q", changed, out)
		}
	})
}

func TestSeedNamesIn(t *testing.T) {
	content := renderSeedBlock("alpha", "a") + "\n" + renderSeedBlock("beta-2", "b") + "\n- notes\n"
	names := seedNamesIn(content)
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta-2" {
		t.Fatalf("names=%v", names)
	}
}

func seedArt(name, scope, seed string) Artifact {
	return Artifact{Type: "subagent", Name: name, Content: "c", MemoryScope: scope, MemorySeed: seed}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestReconcileSeeds(t *testing.T) {
	t.Run("seeds user scope into absent file, idempotent second run", func(t *testing.T) {
		cd := t.TempDir()
		arts := []Artifact{seedArt("rev", "user", "seed body")}
		res, err := ReconcileSeeds(cd, nil, arts, nil)
		if err != nil || res.Written != 1 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		target := filepath.Join(cd, "agent-memory", "rev", "MEMORY.md")
		if got := readFileT(t, target); got != renderSeedBlock("rev", "seed body") {
			t.Fatalf("content=%q", got)
		}
		res, err = ReconcileSeeds(cd, nil, arts, nil)
		if err != nil || res.Written != 0 || res.Unchanged != 1 {
			t.Fatalf("second run must be a no-op: %+v %v", res, err)
		}
	})

	t.Run("update preserves the agent's own notes", func(t *testing.T) {
		cd := t.TempDir()
		_, _ = ReconcileSeeds(cd, nil, []Artifact{seedArt("rev", "user", "v1")}, nil)
		target := filepath.Join(cd, "agent-memory", "rev", "MEMORY.md")
		_ = os.WriteFile(target, []byte(readFileT(t, target)+"\n- agent note\n"), 0o644)
		res, err := ReconcileSeeds(cd, nil, []Artifact{seedArt("rev", "user", "v2")}, nil)
		if err != nil || res.Written != 1 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		got := readFileT(t, target)
		want := renderSeedBlock("rev", "v2") + "\n- agent note\n"
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("project scope seeds every registered project", func(t *testing.T) {
		cd := t.TempDir()
		projA, projB := filepath.Join(cd, "pa"), filepath.Join(cd, "pb")
		_ = os.MkdirAll(projA, 0o755)
		_ = os.MkdirAll(projB, 0o755)
		res, err := ReconcileSeeds(cd, []string{projA, projB}, []Artifact{seedArt("rev", "project", "s")}, nil)
		if err != nil || res.Written != 2 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		for _, p := range []string{projA, projB} {
			if _, err := os.Stat(filepath.Join(p, ".claude", "agent-memory", "rev", "MEMORY.md")); err != nil {
				t.Fatalf("missing seed in %s: %v", p, err)
			}
		}
	})

	t.Run("missing registered project is a warning, not an error", func(t *testing.T) {
		cd := t.TempDir()
		res, err := ReconcileSeeds(cd, []string{filepath.Join(cd, "gone")}, []Artifact{seedArt("rev", "project", "s")}, nil)
		if err != nil || len(res.Warnings) != 1 || res.Written != 0 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("missing registered project warns once regardless of artifact count", func(t *testing.T) {
		cd := t.TempDir()
		arts := []Artifact{seedArt("rev", "project", "s"), seedArt("aux", "project", "s")}
		res, err := ReconcileSeeds(cd, []string{filepath.Join(cd, "gone")}, arts, nil)
		if err != nil || len(res.Warnings) != 1 || res.Written != 0 {
			t.Fatalf("two seeded project-scope artifacts must still dedup to one warning: res=%+v err=%v", res, err)
		}
	})

	t.Run("de-entitlement strips only the block", func(t *testing.T) {
		cd := t.TempDir()
		_, _ = ReconcileSeeds(cd, nil, []Artifact{seedArt("rev", "user", "s")}, nil)
		target := filepath.Join(cd, "agent-memory", "rev", "MEMORY.md")
		_ = os.WriteFile(target, []byte(readFileT(t, target)+"\n- agent note\n"), 0o644)
		res, err := ReconcileSeeds(cd, nil, nil, nil)
		if err != nil || res.Stripped != 1 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if got := readFileT(t, target); got != "- agent note\n" {
			t.Fatalf("agent note must survive alone: %q", got)
		}
	})

	t.Run("scope change user→project strips the old user block", func(t *testing.T) {
		cd := t.TempDir()
		proj := filepath.Join(cd, "proj")
		_ = os.MkdirAll(proj, 0o755)
		_, _ = ReconcileSeeds(cd, []string{proj}, []Artifact{seedArt("rev", "user", "s")}, nil)
		res, err := ReconcileSeeds(cd, []string{proj}, []Artifact{seedArt("rev", "project", "s")}, nil)
		if err != nil || res.Written != 1 || res.Stripped != 1 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if got := readFileT(t, filepath.Join(cd, "agent-memory", "rev", "MEMORY.md")); got != "" {
			t.Fatalf("old user block must be stripped: %q", got)
		}
	})

	t.Run("local scope and skills never seed", func(t *testing.T) {
		cd := t.TempDir()
		res, err := ReconcileSeeds(cd, nil, []Artifact{
			seedArt("loc", "local", "s"),
			{Type: "skill", Name: "sk", Content: "c", MemorySeed: "s"},
		}, nil)

		if err != nil || res.Written != 0 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("unsafe artifact name is rejected", func(t *testing.T) {
		cd := t.TempDir()
		_, err := ReconcileSeeds(cd, nil, []Artifact{seedArt("../evil", "user", "s")}, nil)
		if err == nil {
			t.Fatalf("want traversal rejection")
		}
	})

	t.Run("tampered ledger path outside agent-memory shape is ignored", func(t *testing.T) {
		cd := t.TempDir()
		victim := filepath.Join(cd, "victim.md")
		_ = os.WriteFile(victim, []byte(renderSeedBlock("rev", "s")+"\nprecious\n"), 0o644)
		m := manifest{Seeds: map[string][]string{"rev": {victim}}}
		if err := saveManifest(cd, m, nil); err != nil {
			t.Fatalf("save: %v", err)
		}
		if _, err := ReconcileSeeds(cd, nil, nil, nil); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if got := readFileT(t, victim); !strings.Contains(got, "ORBEAT-SEED:BEGIN rev") {
			t.Fatalf("tampered path must not be touched: %q", got)
		}
	})

	t.Run("ledger records written paths", func(t *testing.T) {
		cd := t.TempDir()
		_, _ = ReconcileSeeds(cd, nil, []Artifact{seedArt("rev", "user", "s")}, nil)
		m, err := loadManifest(cd)
		if err != nil || len(m.Seeds["rev"]) != 1 {
			t.Fatalf("ledger: %+v err=%v", m.Seeds, err)
		}
	})
}

// A directory whose name isn't a valid artifact slug is never treated as a
// seed target by scanSeedFiles — its planted ORBEAT-SEED-shaped block
// survives a full strip run untouched (it was never a target ReconcileSeeds
// itself could have written).
func TestScanSeedFilesIgnoresInvalidSlugDirs(t *testing.T) {
	cd := t.TempDir()
	rogue := filepath.Join(cd, "agent-memory", "Not-A-Slug!")
	_ = os.MkdirAll(rogue, 0o755)
	target := filepath.Join(rogue, "MEMORY.md")
	planted := renderSeedBlock("rev", "seed body") + "\n- notes\n"
	if err := os.WriteFile(target, []byte(planted), 0o644); err != nil {
		t.Fatalf("plant: %v", err)
	}

	if _, err := ReconcileSeeds(cd, nil, nil, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := readFileT(t, target); got != planted {
		t.Fatalf("invalid-slug dir must not be scanned/stripped: %q", got)
	}
}

func TestStripProjectSeeds(t *testing.T) {
	cd := t.TempDir()
	proj := filepath.Join(cd, "proj")
	_ = os.MkdirAll(proj, 0o755)
	arts := []Artifact{seedArt("rev", "project", "s"), seedArt("aux", "user", "s")}
	if _, err := ReconcileSeeds(cd, []string{proj}, arts, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	target := filepath.Join(proj, ".claude", "agent-memory", "rev", "MEMORY.md")
	_ = os.WriteFile(target, []byte(readFileT(t, target)+"\n- agent note\n"), 0o644)

	n, err := StripProjectSeeds(cd, proj)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := readFileT(t, target); got != "- agent note\n" {
		t.Fatalf("agent note must survive: %q", got)
	}
	// The user-scope seed and its ledger entry are untouched; the project's is gone.
	m, _ := loadManifest(cd)
	if len(m.Seeds["aux"]) != 1 || len(m.Seeds["rev"]) != 0 {
		t.Fatalf("ledger after strip: %+v", m.Seeds)
	}
}

// One project's seed target failing non-fatally does not starve the others.
// The target MEMORY.md path is a directory, so this fails in the READ branch
// (os.ReadFile on a directory returns EISDIR, with os.IsNotExist == false) —
// not the write branch; see TestReconcileSeedsWriteFailureIsolated below for
// the write-branch case.
func TestReconcileSeedsReadFailureIsolated(t *testing.T) {
	claudeDir := t.TempDir()
	good := t.TempDir()
	bad := t.TempDir()
	// In `bad`, make the target MEMORY.md path a directory so the read fails (EISDIR).
	badTarget := filepath.Join(bad, ".claude", "agent-memory", "rev", "MEMORY.md")
	if err := os.MkdirAll(badTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	art := Artifact{Type: "subagent", Name: "rev", Content: "body", MemoryScope: "project", MemorySeed: "SEED"}
	res, err := ReconcileSeeds(claudeDir, []string{good, bad}, []Artifact{art}, nil)
	if err != nil {
		t.Fatalf("per-target I/O failure must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("want 1 failure, got %v", res.Failures)
	}
	goodTarget := filepath.Join(good, ".claude", "agent-memory", "rev", "MEMORY.md")
	if _, statErr := os.Stat(goodTarget); statErr != nil {
		t.Fatalf("the healthy project's seed must still be written: %v", statErr)
	}
}

// A per-target WRITE failure (as opposed to a read failure) is likewise isolated.
func TestReconcileSeedsWriteFailureIsolated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	claudeDir := t.TempDir()
	proj := t.TempDir()
	art := Artifact{Type: "subagent", Name: "rev", Content: "b", MemoryScope: "project", MemorySeed: "SEED"}
	// Run 1: create the seeded file normally.
	if _, err := ReconcileSeeds(claudeDir, []string{proj}, []Artifact{art}, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(proj, ".claude", "agent-memory", "rev")
	// Make the parent unwritable: the file stays READABLE (so the read branch
	// passes) but writeFileAtomic's CreateTemp fails -> the WRITE branch fires.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	// Run 2: change the seed so a write is actually attempted.
	art.MemorySeed = "SEED-V2"
	res, err := ReconcileSeeds(claudeDir, []string{proj}, []Artifact{art}, nil)
	if err != nil {
		t.Fatalf("per-target write failure must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 || res.Written != 0 {
		t.Fatalf("want 1 failure + 0 written, got failures=%v written=%d", res.Failures, res.Written)
	}
	if !strings.Contains(res.Failures[0], "write") {
		t.Fatalf("failure should name the write branch, got %q", res.Failures[0])
	}
}

// An unsafe artifact name is fatal.
func TestReconcileSeedsUnsafeNameIsFatal(t *testing.T) {
	_, err := ReconcileSeeds(t.TempDir(), nil, []Artifact{
		{Type: "subagent", Name: "../evil", MemoryScope: "user", MemorySeed: "S"},
	}, nil)

	if err == nil || !isFatal(err) {
		t.Fatalf("unsafe name must be fatal, got %v", err)
	}
}

// A strip that fails non-fatally must keep its prior ledger entry, or a project
// de-registered before the next successful sync orphans the block forever.
func TestReconcileSeedsStripFailurePreservesLedger(t *testing.T) {
	claudeDir := t.TempDir()
	proj := t.TempDir()
	art := Artifact{Type: "subagent", Name: "rev", Content: "b", MemoryScope: "project", MemorySeed: "SEED"}
	// Run 1: seed written, ledger records the path.
	if _, err := ReconcileSeeds(claudeDir, []string{proj}, []Artifact{art}, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(proj, ".claude", "agent-memory", "rev", "MEMORY.md")
	// Replace the seeded file with a directory so the strip pass's read fails (EISDIR).
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// Run 2: de-entitle -> strip pass runs against the ledger path and fails.
	res, err := ReconcileSeeds(claudeDir, []string{proj}, nil, nil)
	if err != nil {
		t.Fatalf("strip I/O failure must be non-fatal: %v", err)
	}
	if len(res.Failures) == 0 {
		t.Fatal("want a recorded failure for the unreadable strip target")
	}
	m, err := loadManifest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range m.Seeds["rev"] {
		if p == target {
			found = true
		}
	}
	if !found {
		t.Fatalf("strip-failed path must stay in the Seeds ledger for retry; ledger=%v", m.Seeds)
	}
}

// A single unreadable target must be reported once, not twice — the write
// pass and the strip pass both encounter the same broken path within one run,
// and the strip pass must skip it rather than re-read and re-report it.
func TestReconcileSeedsFailedPathNotDoubleCounted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	claudeDir := t.TempDir()
	proj := t.TempDir()
	art := Artifact{Type: "subagent", Name: "rev", Content: "b", MemoryScope: "project", MemorySeed: "SEED"}
	// Run 1: seed written normally.
	if _, err := ReconcileSeeds(claudeDir, []string{proj}, []Artifact{art}, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(proj, ".claude", "agent-memory", "rev", "MEMORY.md")
	if err := os.Chmod(target, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o644) })

	// Run 2: still entitled (still a write target) AND the file is a strip
	// candidate too (fs-scan finds it, still present on disk).
	art.MemorySeed = "SEED-V2"
	res, err := ReconcileSeeds(claudeDir, []string{proj}, []Artifact{art}, nil)
	if err != nil {
		t.Fatalf("permission failure must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("one broken file must be reported once, not twice: %v", res.Failures)
	}
}

// One unstrippable candidate does not prevent other candidates being stripped.
func TestReconcileSeedsStripFailureIsolated(t *testing.T) {
	claudeDir := t.TempDir()
	a := t.TempDir()
	b := t.TempDir()
	art := Artifact{Type: "subagent", Name: "rev", Content: "c", MemoryScope: "project", MemorySeed: "SEED"}
	if _, err := ReconcileSeeds(claudeDir, []string{a, b}, []Artifact{art}, nil); err != nil {
		t.Fatal(err)
	}
	badTarget := filepath.Join(a, ".claude", "agent-memory", "rev", "MEMORY.md")
	goodTarget := filepath.Join(b, ".claude", "agent-memory", "rev", "MEMORY.md")
	if err := os.Remove(badTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(badTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	// De-entitle: both should be stripped; `a` fails, `b` must still be stripped.
	res, err := ReconcileSeeds(claudeDir, []string{a, b}, nil, nil)
	if err != nil {
		t.Fatalf("strip failure must be non-fatal: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("want exactly 1 failure, got %v", res.Failures)
	}
	if res.Stripped != 1 {
		t.Fatalf("the healthy project's block must still be stripped; stripped=%d", res.Stripped)
	}
	if strings.Contains(readFileT(t, goodTarget), "ORBEAT-SEED") {
		t.Fatal("healthy target still has its block — strip isolation broken")
	}
}

func TestStripProjectSeedsIgnoresSiblingPrefix(t *testing.T) {
	cd := t.TempDir()
	proj := filepath.Join(cd, "proj")
	sibling := filepath.Join(cd, "proj-other")
	_ = os.MkdirAll(proj, 0o755)
	_ = os.MkdirAll(sibling, 0o755)
	arts := []Artifact{seedArt("rev", "project", "s")}
	if _, err := ReconcileSeeds(cd, []string{proj, sibling}, arts, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := StripProjectSeeds(cd, proj)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	// The sibling with the common name prefix must be untouched: block on disk + ledger entry.
	sibTarget := filepath.Join(sibling, ".claude", "agent-memory", "rev", "MEMORY.md")
	if got := readFileT(t, sibTarget); !strings.Contains(got, "ORBEAT-SEED:BEGIN rev") {
		t.Fatalf("sibling block must survive: %q", got)
	}
	m, _ := loadManifest(cd)
	if len(m.Seeds["rev"]) != 1 || m.Seeds["rev"][0] != sibTarget {
		t.Fatalf("sibling ledger entry must survive: %+v", m.Seeds)
	}
}

// S1 (audit finding, reproduced): an orphan BEGIN marker (no matching END)
// sitting above a LATER genuine block for the same name lets seedBlockRe span
// from the orphan BEGIN all the way to that later block's own END — an
// in-place splice then deletes everything in between, including the agent's
// own notes. A changed seed body is required to trigger the splice path (a
// no-op body never reaches the destructive branch). On unfixed code this
// test fails with the notes gone.
func TestReconcileSeedsSkipsMalformedMarkerOnWrite(t *testing.T) {
	cd := t.TempDir()
	target := filepath.Join(cd, "agent-memory", "rev", "MEMORY.md")
	must(t, os.MkdirAll(filepath.Dir(target), 0o755))

	orphan := "<!-- ORBEAT-SEED:BEGIN rev sha=deadbeef0000 x -->\n"
	notes := "- precious agent notes\n- more notes\n"
	genuine := renderSeedBlock("rev", "seed v1")
	existing := orphan + notes + genuine
	must(t, os.WriteFile(target, []byte(existing), 0o644))

	res, err := ReconcileSeeds(cd, nil, []Artifact{seedArt("rev", "user", "seed v2")}, nil)
	if err != nil {
		t.Fatalf("must be non-fatal: %v", err)
	}

	got := readFileT(t, target)
	if got != existing {
		t.Fatalf("malformed file must be left untouched:\nwant: %q\ngot:  %q", existing, got)
	}
	if !strings.Contains(got, "precious agent notes") {
		t.Fatal("agent notes must survive")
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "rev") {
		t.Fatalf("expected exactly one malformed-marker warning naming the artifact, got %v", res.Warnings)
	}
	m, err := loadManifest(cd)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Seeds["rev"]) != 1 || m.Seeds["rev"][0] != target {
		t.Fatalf("target must keep a ledger entry so a later run retries after manual repair; ledger=%v", m.Seeds)
	}
}

// Mirror of TestReconcileSeedsSkipsMalformedMarkerOnWrite for the strip
// (de-entitlement) path: a file corrupted with an orphan BEGIN above a
// genuine block must not be spliced by the strip pass either, and the ledger
// entry must survive so a later run retries after manual repair.
func TestReconcileSeedsDeEntitlementSkipsMalformedMarker(t *testing.T) {
	cd := t.TempDir()
	if _, err := ReconcileSeeds(cd, nil, []Artifact{seedArt("rev", "user", "v1")}, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cd, "agent-memory", "rev", "MEMORY.md")
	genuine := readFileT(t, target)
	orphan := "<!-- ORBEAT-SEED:BEGIN rev sha=deadbeef0000 x -->\n"
	notes := "- precious agent notes\n"
	corrupted := orphan + notes + genuine
	must(t, os.WriteFile(target, []byte(corrupted), 0o644))

	// De-entitle: the strip pass must now see "rev" as undesired.
	res, err := ReconcileSeeds(cd, nil, nil, nil)
	if err != nil {
		t.Fatalf("must be non-fatal: %v", err)
	}
	if res.Stripped != 0 {
		t.Fatalf("malformed file must not be spliced, got Stripped=%d", res.Stripped)
	}
	got := readFileT(t, target)
	if got != corrupted {
		t.Fatalf("malformed file must be left untouched:\nwant: %q\ngot:  %q", corrupted, got)
	}
	if !strings.Contains(got, "precious agent notes") {
		t.Fatal("agent notes must survive")
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "rev") {
		t.Fatalf("expected exactly one malformed-marker warning naming the artifact, got %v", res.Warnings)
	}
	m, err := loadManifest(cd)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Seeds["rev"]) != 1 || m.Seeds["rev"][0] != target {
		t.Fatalf("de-entitled but malformed target must keep its ledger entry for retry; ledger=%v", m.Seeds)
	}
}

// Mirror of the strip-path malformed-marker guard for StripProjectSeeds (the
// `project remove` path): a malformed marker must block the splice AND keep
// the ledger entry, rather than unconditionally forgetting it the way a
// normal (healthy) project removal does.
func TestStripProjectSeedsPreservesLedgerOnMalformedMarker(t *testing.T) {
	cd := t.TempDir()
	proj := filepath.Join(cd, "proj")
	must(t, os.MkdirAll(proj, 0o755))
	if _, err := ReconcileSeeds(cd, []string{proj}, []Artifact{seedArt("rev", "project", "v1")}, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(proj, ".claude", "agent-memory", "rev", "MEMORY.md")
	genuine := readFileT(t, target)
	orphan := "<!-- ORBEAT-SEED:BEGIN rev sha=deadbeef0000 x -->\n"
	notes := "- precious agent notes\n"
	corrupted := orphan + notes + genuine
	must(t, os.WriteFile(target, []byte(corrupted), 0o644))

	n, err := StripProjectSeeds(cd, proj)
	if err != nil {
		t.Fatalf("must be non-fatal: %v", err)
	}
	if n != 0 {
		t.Fatalf("malformed file must not be spliced, got n=%d", n)
	}
	got := readFileT(t, target)
	if got != corrupted {
		t.Fatalf("malformed file must be left untouched:\nwant: %q\ngot:  %q", corrupted, got)
	}
	m, err := loadManifest(cd)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Seeds["rev"]) != 1 || m.Seeds["rev"][0] != target {
		t.Fatalf("malformed target must keep its ledger entry after project remove; ledger=%v", m.Seeds)
	}
}

// B24: one unreadable candidate must not stop StripProjectSeeds from
// finishing the OTHERS, or from saving the manifest for the ones that DID
// succeed. Before this fix, the strip loop returned immediately on the FIRST
// per-candidate error — skipping every remaining candidate — and, because
// that early return sat above the ledger-cleanup + saveManifest call, even a
// candidate that succeeded and was already rewritten on disk never got its
// ledger entry updated.
//
// Go's map iteration order over `candidates` is randomized, so the assertion
// has to hold regardless of which candidate the loop reaches first. Checking
// the SAVED manifest (not the return value, and not the file alone) is what
// makes this discriminating either way: on the unfixed code, if the bad
// candidate is reached first the good one is never even attempted, and if the
// good one is reached first its write lands on disk but the save that would
// record it in the ledger never runs, because the function returns before
// reaching it — so under BOTH orderings the saved ledger fails to reflect
// reality on the unfixed code, while it always does on the fixed code.
func TestStripProjectSeedsIsolatesAPerCandidateFailure(t *testing.T) {
	claudeDir := t.TempDir()
	proj := t.TempDir()

	// goodagent: a real, healthy, project-scope seed via the ordinary path.
	if _, err := ReconcileSeeds(claudeDir, []string{proj}, []Artifact{seedArt("goodagent", "project", "body")}, nil); err != nil {
		t.Fatalf("seed goodagent: %v", err)
	}
	goodPath := filepath.Join(proj, ".claude", "agent-memory", "goodagent", "MEMORY.md")
	if !strings.Contains(readFileT(t, goodPath), "ORBEAT-SEED:BEGIN goodagent") {
		t.Fatal("precondition: goodagent's block never landed")
	}

	// badagent: a ledger entry shaped like a real MEMORY.md path, but a
	// DIRECTORY on disk — reproduces "one unreadable MEMORY.md" (EISDIR)
	// without needing root-independent permission bits.
	badPath := filepath.Join(proj, ".claude", "agent-memory", "badagent", "MEMORY.md")
	must(t, os.MkdirAll(badPath, 0o755))
	m, err := loadManifest(claudeDir)
	must(t, err)
	m.Seeds["badagent"] = []string{badPath}
	must(t, saveManifest(claudeDir, m, nil))

	if _, err := StripProjectSeeds(claudeDir, proj); err == nil {
		t.Fatal("a genuinely unreadable candidate must be reported, not silently dropped")
	}

	m2, err := loadManifest(claudeDir)
	must(t, err)
	if _, ok := m2.Seeds["goodagent"]; ok {
		t.Fatalf("the healthy candidate must be stripped AND saved out of the ledger regardless of which candidate the loop reaches first; ledger=%v", m2.Seeds)
	}
	if _, ok := m2.Seeds["badagent"]; !ok {
		t.Fatalf("the failed candidate must keep its ledger entry so a later run retries it; ledger=%v", m2.Seeds)
	}
}

// Regression guard: a legitimate single well-formed block — planted directly
// rather than via a prior ReconcileSeeds run — must still merge and strip
// normally; the malformed-marker gate must not false-positive on it.
func TestReconcileSeedsHealthyMarkerMergesAndStripsNormally(t *testing.T) {
	cd := t.TempDir()
	target := filepath.Join(cd, "agent-memory", "rev", "MEMORY.md")
	must(t, os.MkdirAll(filepath.Dir(target), 0o755))
	existing := renderSeedBlock("rev", "v1") + "- my own note\n"
	must(t, os.WriteFile(target, []byte(existing), 0o644))

	res, err := ReconcileSeeds(cd, nil, []Artifact{seedArt("rev", "user", "v2")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 1 || len(res.Warnings) != 0 {
		t.Fatalf("expected a normal write with no warnings, got %+v", res)
	}
	// mergeSeed always re-inserts exactly one blank line between the
	// re-hoisted block and whatever follows (see its doc comment).
	want := renderSeedBlock("rev", "v2") + "\n- my own note\n"
	if got := readFileT(t, target); got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	res2, err := ReconcileSeeds(cd, nil, nil, nil) // de-entitle -> strip
	if err != nil {
		t.Fatal(err)
	}
	if res2.Stripped != 1 || len(res2.Warnings) != 0 {
		t.Fatalf("expected a normal strip with no warnings, got %+v", res2)
	}
	if got := readFileT(t, target); got != "- my own note\n" {
		t.Fatalf("agent note must survive alone: %q", got)
	}
}

func TestSeedMarkersHealthy(t *testing.T) {
	t.Run("no block is healthy", func(t *testing.T) {
		if !seedMarkersHealthy("- just notes\n", "rev") {
			t.Fatal("expected healthy")
		}
	})

	t.Run("single well-formed block is healthy", func(t *testing.T) {
		block := renderSeedBlock("rev", "body")
		if !seedMarkersHealthy(block+"- notes\n", "rev") {
			t.Fatal("expected healthy")
		}
	})

	t.Run("orphan BEGIN with no END is unhealthy", func(t *testing.T) {
		orphan := "<!-- ORBEAT-SEED:BEGIN rev sha=abc123abc123 x -->\n- notes\n"
		if seedMarkersHealthy(orphan, "rev") {
			t.Fatal("expected unhealthy")
		}
	})

	t.Run("orphan BEGIN above a later genuine block is unhealthy", func(t *testing.T) {
		orphan := "<!-- ORBEAT-SEED:BEGIN rev sha=abc123abc123 x -->\n"
		content := orphan + "- notes\n" + renderSeedBlock("rev", "body")
		if seedMarkersHealthy(content, "rev") {
			t.Fatal("expected unhealthy")
		}
	})

	t.Run("duplicate full blocks are unhealthy", func(t *testing.T) {
		block := renderSeedBlock("rev", "body")
		if seedMarkersHealthy(block+block, "rev") {
			t.Fatal("expected unhealthy")
		}
	})

	t.Run("dangling END with no BEGIN is unhealthy", func(t *testing.T) {
		dangling := "- notes\n<!-- ORBEAT-SEED:END rev -->\n"
		if seedMarkersHealthy(dangling, "rev") {
			t.Fatal("expected unhealthy")
		}
	})

	t.Run("name-scoped: another name's orphan marker does not affect this name's health", func(t *testing.T) {
		otherOrphan := "<!-- ORBEAT-SEED:BEGIN other sha=abc123abc123 x -->\n"
		if !seedMarkersHealthy(otherOrphan+"- notes\n", "rev") {
			t.Fatal("expected healthy for a name uninvolved in the other name's orphan")
		}
	})

	t.Run("name is not a prefix trap", func(t *testing.T) {
		// An orphan for "rev-two" must not be counted against "rev".
		orphan := "<!-- ORBEAT-SEED:BEGIN rev-two sha=abc123abc123 x -->\n"
		if !seedMarkersHealthy(orphan, "rev") {
			t.Fatal("expected healthy: orphan belongs to a different (prefix-colliding) name")
		}
	})
}

// Direct coverage of stripUndesired's malformed-marker gate — mirrors
// TestMergeRulesFileSkipsMalformedMarkers's granularity on the rules side,
// one level below the ReconcileSeeds/StripProjectSeeds integration tests.
func TestStripUndesiredSkipsMalformedMarkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MEMORY.md")
	orphan := "<!-- ORBEAT-SEED:BEGIN rev sha=deadbeef0000 x -->\n"
	notes := "- precious agent notes\n"
	genuine := renderSeedBlock("rev", "seed body")
	content := orphan + notes + genuine
	must(t, os.WriteFile(path, []byte(content), 0o644))

	r, err := openRooted(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n, warnings, err := stripUndesired(r, path, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(warnings) != 1 {
		t.Fatalf("expected skip+one warning, got n=%d warnings=%v", n, warnings)
	}
	after, _ := os.ReadFile(path)
	if string(after) != content {
		t.Fatalf("malformed file was modified:\n%s", after)
	}
}

// A file with an unhealthy block for one name but a healthy block for
// another must still strip the healthy one — the malformed-marker gate is
// name-scoped, not whole-file.
func TestStripUndesiredIsolatesUnhealthyNameFromHealthyOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MEMORY.md")
	orphan := "<!-- ORBEAT-SEED:BEGIN rev sha=deadbeef0000 x -->\n"
	content := orphan + renderSeedBlock("rev", "body") + renderSeedBlock("aux", "body2")
	must(t, os.WriteFile(path, []byte(content), 0o644))

	r, err := openRooted(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n, warnings, err := stripUndesired(r, path, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(warnings) != 1 {
		t.Fatalf("expected the healthy 'aux' block stripped and 'rev' warned, got n=%d warnings=%v", n, warnings)
	}
	got := readFileT(t, path)
	if strings.Contains(got, "ORBEAT-SEED:BEGIN aux") {
		t.Fatalf("healthy 'aux' block must be stripped: %q", got)
	}
	if !strings.Contains(got, "ORBEAT-SEED:BEGIN rev") {
		t.Fatalf("unhealthy 'rev' markers must survive untouched: %q", got)
	}
}

func TestReconcileSeedsPlanModeWritesNothing(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	arts := []Artifact{{
		Name: "alpha", Type: "subagent", Content: "BODY",
		MemorySeed: "SEED-BODY", MemoryScope: "project",
	}}
	if _, err := ReconcileSeeds(home, []string{proj}, arts, nil); err != nil {
		t.Fatal(err)
	}
	beforeProj, beforeHome := treeSnapshot(t, proj), treeSnapshot(t, home)

	arts[0].MemorySeed = "SEED-BODY-CHANGED"
	var p Plan
	res, err := ReconcileSeeds(home, []string{proj}, arts, &p)
	if err != nil {
		t.Fatalf("plan run must not error: %v", err)
	}
	assertTreeUnchanged(t, "project", proj, beforeProj)
	assertTreeUnchanged(t, "sync root (manifest must not be rewritten either)", home, beforeHome)
	if res.Written != 1 {
		t.Errorf("counter must describe the plan: written=%d, want 1", res.Written)
	}
	// The seed write plus the manifest write. Assert WHICH, not just how many:
	// a bare count passes if the manifest were recorded twice and the seed not at all.
	var sawSeed, sawManifest bool
	for _, c := range p.Changes() {
		if strings.HasSuffix(c.Path, manifestName) {
			sawManifest = true
		} else if strings.HasPrefix(c.Path, proj) {
			sawSeed = true
		}
	}
	if !sawSeed || !sawManifest {
		t.Errorf("plan must record the seed write and the manifest: seed=%v manifest=%v changes=%+v",
			sawSeed, sawManifest, p.Changes())
	}
}

// TestReconcileSeedsPlanModeAgainstAbsentRoot is B1's seeds half: a plan
// against a sync root (home) that has never existed must plan the user-scope
// target as a create, not report a bogus "containment root … vanished"
// failure — the bug reproduced with 2 file-backed artifacts + 1 user-scope
// seed + 1 rule against an absent home, where only the rules section (which
// never touches claudeDir at all) came back correct. A registered project
// that DOES already exist is included to prove the fix is scoped to the
// claudeDir/user-scope boundary specifically: project-scope handling, which
// was never broken, must stay exactly as it was.
func TestReconcileSeedsPlanModeAgainstAbsentRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), "never-synced")
	proj := t.TempDir()
	arts := []Artifact{
		{Name: "alpha", Type: "subagent", Content: "A", MemoryScope: "user", MemorySeed: "USER-BODY"},
		{Name: "beta", Type: "subagent", Content: "B", MemoryScope: "project", MemorySeed: "PROJ-BODY"},
	}

	var p Plan
	res, err := ReconcileSeeds(home, []string{proj}, arts, &p)
	if err != nil {
		t.Fatalf("plan run against an absent sync root must not error: %v", err)
	}
	if _, statErr := os.Stat(home); !os.IsNotExist(statErr) {
		t.Fatalf("plan mode must not create the sync root, stat err=%v", statErr)
	}
	if res.Written != 2 {
		t.Fatalf("both seed targets must plan as written: res=%+v", res)
	}
	if len(res.Failures) != 0 {
		t.Fatalf("an absent sync root must not be reported as a vanished containment root: %v", res.Failures)
	}

	changes := p.Changes()
	if len(changes) != 3 { // alpha's user-scope seed + beta's project-scope seed + the manifest
		t.Fatalf("want 3 recorded changes, got %+v", changes)
	}
	wantSuffixes := map[string]bool{
		filepath.Join("agent-memory", "alpha", "MEMORY.md"):           false,
		filepath.Join(".claude", "agent-memory", "beta", "MEMORY.md"): false,
		manifestName: false,
	}
	for _, c := range changes {
		if c.Op != OpCreate {
			t.Errorf("change %+v: want op=create against a never-synced root", c)
		}
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
		wantSuffixes[matched] = true
	}
	for suffix, seen := range wantSuffixes {
		if !seen {
			t.Fatalf("expected a recorded change ending in %q, got %+v", suffix, changes)
		}
	}
}

// TestReconcileSeedsPlanVsApply_ForeignNestedBlockDiverges pins spec §8's
// read-after-write hazard (N1): the strip pass's re-read of a path the write
// pass just (recorded-not-)wrote CAN produce a wrong plan, given an on-disk
// shape orbeat itself never writes — a foreign block for a different name
// physically nested inside the block being rewritten.
//
// Here "alpha" is still entitled and its body changes, so the write pass
// replaces its whole marker span (mergeSeed drops loc[0]:loc[1] and hoists a
// fresh block to the top) — which, on this crafted fixture, also destroys a
// "beta" block that happens to sit inside that span. A real (apply) run
// performs that write for real, so by the time the strip pass re-reads the
// file, "beta" is already gone and there is nothing left to strip. Plan mode
// never performed the write, so the strip pass's re-read sees the ORIGINAL
// content, still finds "beta", and — correctly, given what it can see —
// strips it. The two runs report different Stripped counts for content that
// only ever existed in this one contrived shape.
//
// orbeat's own writer never produces this: every block it writes is hoisted
// to the top of the file, never nested inside another name's markers. This
// test exists so the invariant spec §8 relies on ("no undesired name's block
// lives in a path the write pass also touched") has a test that fails the
// day something makes this shape reachable through normal use — the gap
// §7 gate 2 (TestPlanMatchesApply_ReconcileSeeds) does not cover, because
// its fixtures never nest one name's block inside another's.
func TestReconcileSeedsPlanVsApply_ForeignNestedBlockDiverges(t *testing.T) {
	nested := "<!-- ORBEAT-SEED:BEGIN alpha sha=000000000000 — managed by orbeat-sync; edit BELOW this block -->\n" +
		renderSeedBlock("beta", "BETA-BODY") +
		"old alpha body\n" +
		"<!-- ORBEAT-SEED:END alpha -->\n"
	arts := []Artifact{{Name: "alpha", Type: "subagent", MemoryScope: "project", MemorySeed: "BODY-A2"}}

	plant := func(t *testing.T) (home, proj string) {
		t.Helper()
		home, proj = t.TempDir(), t.TempDir()
		target := filepath.Join(proj, ".claude", "agent-memory", "alpha", "MEMORY.md")
		must(t, os.MkdirAll(filepath.Dir(target), 0o755))
		must(t, os.WriteFile(target, []byte(nested), 0o644))
		return home, proj
	}

	homeApply, projApply := plant(t)
	applyRes, err := ReconcileSeeds(homeApply, []string{projApply}, arts, nil)
	if err != nil {
		t.Fatalf("apply run: %v", err)
	}
	if applyRes.Stripped != 0 {
		t.Fatalf("apply must find nothing left to strip (the write already destroyed the nested foreign block): Stripped=%d", applyRes.Stripped)
	}

	homePlan, projPlan := plant(t)
	var p Plan
	planRes, err := ReconcileSeeds(homePlan, []string{projPlan}, arts, &p)
	if err != nil {
		t.Fatalf("plan run: %v", err)
	}
	if planRes.Stripped != 1 {
		t.Fatalf("plan mode's stale re-read must still see and strip the foreign 'beta' block: Stripped=%d", planRes.Stripped)
	}

	if applyRes.Stripped == planRes.Stripped {
		t.Fatalf("this fixture is meant to demonstrate a divergence; got the same Stripped=%d on both sides — has the invariant this pins changed?", applyRes.Stripped)
	}
}

// A manifest seed entry lying under NEITHER claudeDir NOR any registered
// project must be left completely alone. This is the shape a tampered
// ~/.claude/.orbeat-sync-manifest.json produces: the path passes
// validSeedPath (absolute, and shaped .../agent-memory/<slug>/MEMORY.md) yet
// names a file the user never asked orbeat-sync to manage.
//
// The fixture is built so that a containment root DERIVED from the path (four
// filepath.Dir calls, which is what the strip pass used to do) lands on base,
// a directory that exists and that os.OpenRoot therefore accepts. That is the
// whole defect: a root derived from an untrusted path is by construction an
// ancestor of it, so rooted.rel can never refuse the operation. The root has
// to come from the set this run was handed, claudeDir plus the registered
// projects, and a path under none of them is not orbeat-sync's to touch.
func TestReconcileSeedsLeavesALedgerPathOutsideEveryTrustedRootAlone(t *testing.T) {
	base := t.TempDir()
	cd := filepath.Join(base, "home", ".claude")
	must(t, os.MkdirAll(cd, 0o755))
	proj := filepath.Join(base, "registered-project")
	must(t, os.MkdirAll(proj, 0o755))

	victim := filepath.Join(base, "elsewhere", "agent-memory", "rev", "MEMORY.md")
	must(t, os.MkdirAll(filepath.Dir(victim), 0o755))
	content := renderSeedBlock("rev", "governed body") + "\nprecious developer content\n"
	must(t, os.WriteFile(victim, []byte(content), 0o644))
	must(t, saveManifest(cd, manifest{Seeds: map[string][]string{"rev": {victim}}}, nil))

	// No artifacts, so every ledger entry is undesired and the strip pass
	// runs against exactly this path.
	res, err := ReconcileSeeds(cd, []string{proj}, nil, nil)
	if err != nil {
		t.Fatalf("an untrusted ledger entry is a skip, not an abort: %v", err)
	}
	if got := readFileT(t, victim); got != content {
		t.Fatalf("a path under no trusted root must not be touched:\nwant: %q\ngot:  %q", content, got)
	}
	if res.Stripped != 0 {
		t.Fatalf("nothing outside a trusted root may be stripped, got Stripped=%d", res.Stripped)
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
}

// The entry the run refused to touch keeps its ledger line. The block may
// still be on disk and this run did not verify otherwise, so the same
// preservation rule the rest of ReconcileSeeds applies to a unit it could not
// complete applies here: over-recording falls out by itself the first run
// that can see the path, under-recording is permanent.
func TestReconcileSeedsPreservesTheLedgerEntryItRefusedToTouch(t *testing.T) {
	base := t.TempDir()
	cd := filepath.Join(base, "home", ".claude")
	must(t, os.MkdirAll(cd, 0o755))
	victim := filepath.Join(base, "elsewhere", "agent-memory", "rev", "MEMORY.md")
	must(t, os.MkdirAll(filepath.Dir(victim), 0o755))
	must(t, os.WriteFile(victim, []byte(renderSeedBlock("rev", "governed body")), 0o644))
	must(t, saveManifest(cd, manifest{Seeds: map[string][]string{"rev": {victim}}}, nil))

	if _, err := ReconcileSeeds(cd, nil, nil, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	m, err := loadManifest(cd)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Seeds["rev"]) != 1 || m.Seeds["rev"][0] != victim {
		t.Fatalf("a skipped entry must stay in the ledger for a run that can see it: ledger=%v", m.Seeds)
	}
}
