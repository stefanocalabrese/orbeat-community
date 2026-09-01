package syncclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Symlink containment (audit S2). These pin that every orbeat-sync writer is
// contained beneath its os.Root boundary. The genuinely reproducing escape is
// an INTERMEDIATE directory symlink (a component of the write path, not the
// leaf): against the unfixed writers the reproductions below land bytes OUTSIDE
// the boundary; with os.Root the operation fails and is recorded per-unit
// (non-fatal), the healthy units still sync, and the ledger is preserved.
//
// NOTE on the audit's "dangling symlink at target" (escape #1): it does NOT
// reproduce against the current writer. writeFileAtomic/rooted.writeAtomic write
// via a same-dir temp + rename, and rename replaces a symlink AT the target in
// place rather than following it — so the audit's stale os.WriteFile reference
// (reconcile.go:126) no longer describes the code. TestReconcileDanglingLeaf...
// documents that (green before and after — it is an invariant guard, not a
// red-proof). The two writers still differ on a leaf symlink and both stay
// contained; see the per-test comments.

// outsidePath returns a path guaranteed to sit outside claudeDir (a sibling temp
// dir), for asserting "nothing escaped".
func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Fatalf("symlink %s -> %s: %v", newname, oldname, err)
	}
}

func assertNoFile(t *testing.T, p, msg string) {
	t.Helper()
	if _, err := os.Stat(p); err == nil {
		t.Fatalf("SECURITY: %s — a file exists at %s", msg, p)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error at %s: %v", p, err)
	}
}

// RED-PROOF (intermediate dir symlink, Reconcile). An escaping `skills` symlink
// makes the unfixed writer create skills/<name>/SKILL.md at the symlink's
// outside target. With os.Root the write fails, is recorded, and a healthy
// subagent (agents/ is untouched) still syncs.
func TestReconcileIntermediateSymlinkContained(t *testing.T) {
	claudeDir := t.TempDir()
	outside := t.TempDir()
	outsideSkills := filepath.Join(outside, "skills")
	must(t, os.MkdirAll(outsideSkills, 0o755))
	mustSymlink(t, outsideSkills, filepath.Join(claudeDir, "skills"))

	res, err := Reconcile(claudeDir, []Artifact{
		{Type: "skill", Name: "esc", Content: "PWNED"},
		{Type: "subagent", Name: "good", Content: "G"},
	}, nil)
	if err != nil {
		t.Fatalf("a symlink escape must be non-fatal (per-unit), got %v", err)
	}
	assertNoFile(t, filepath.Join(outsideSkills, "esc", "SKILL.md"), "skill escaped the sync root")
	if len(res.Failures) != 1 {
		t.Fatalf("want exactly 1 recorded failure (the escaping skill), got %v", res.Failures)
	}
	if read(t, filepath.Join(claudeDir, "agents", "good.md")) != "G" {
		t.Fatal("the healthy subagent must still sync")
	}
}

// RED-PROOF + ledger preservation. When the escaping unit was previously managed
// (its rel is in the ledger), the failed write must KEEP the ledger entry so a
// later run retries, exactly like any other per-unit write failure.
func TestReconcileIntermediateSymlinkPreservesLedger(t *testing.T) {
	claudeDir := t.TempDir()
	outside := t.TempDir()
	outsideSkills := filepath.Join(outside, "skills")
	must(t, os.MkdirAll(outsideSkills, 0o755))
	// Ledger says we already manage skills/esc/SKILL.md.
	must(t, os.WriteFile(filepath.Join(claudeDir, manifestName),
		[]byte(`{"files":["skills/esc/SKILL.md"]}`), 0o644))
	// The skills dir is then hijacked to an escaping symlink.
	mustSymlink(t, outsideSkills, filepath.Join(claudeDir, "skills"))

	res, err := Reconcile(claudeDir, []Artifact{{Type: "skill", Name: "esc", Content: "V2"}}, nil)
	if err != nil {
		t.Fatalf("non-fatal expected, got %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("want 1 failure, got %v", res.Failures)
	}
	assertNoFile(t, filepath.Join(outsideSkills, "esc", "SKILL.md"), "skill escaped the sync root")
	m, err := loadManifest(claudeDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range m.Files {
		if f == "skills/esc/SKILL.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("write-failed managed file must stay in the ledger for retry; ledger=%v", m.Files)
	}
}

// RED-PROOF (intermediate dir symlink, Seeds). A registered project whose
// `.claude/agent-memory` is an escaping symlink makes the unfixed seed writer
// land MEMORY.md outside the repo. With os.Root the write fails, is recorded,
// nothing escapes, and a second, healthy project still gets its seed.
func TestReconcileSeedsIntermediateSymlinkContained(t *testing.T) {
	claudeDir := t.TempDir()
	badProj := t.TempDir()
	goodProj := t.TempDir()
	outside := t.TempDir()
	outsideMem := filepath.Join(outside, "agent-memory")
	must(t, os.MkdirAll(outsideMem, 0o755))
	must(t, os.MkdirAll(filepath.Join(badProj, ".claude"), 0o755))
	mustSymlink(t, outsideMem, filepath.Join(badProj, ".claude", "agent-memory"))

	res, err := ReconcileSeeds(claudeDir, []string{badProj, goodProj}, []Artifact{
		{Type: "subagent", Name: "rev", Content: "A", MemoryScope: "project", MemorySeed: "SEEDBODY"},
	}, nil)

	if err != nil {
		t.Fatalf("a symlink escape must be non-fatal (per-unit), got %v", err)
	}
	assertNoFile(t, filepath.Join(outsideMem, "rev", "MEMORY.md"), "seed escaped the project root")
	if len(res.Failures) != 1 {
		t.Fatalf("want exactly 1 recorded failure (the escaping project), got %v", res.Failures)
	}
	if res.Written != 1 {
		t.Fatalf("the healthy project's seed must still be written (Written=%d)", res.Written)
	}
	good := readFileT(t, filepath.Join(goodProj, ".claude", "agent-memory", "rev", "MEMORY.md"))
	if !strings.Contains(good, "SEEDBODY") {
		t.Fatalf("healthy project seed missing its body: %q", good)
	}
}

// Rules leaf-symlink hardening. Rules files (AGENTS.md/CLAUDE.md) are direct
// children of the registered project root, so there is no INTERMEDIATE-dir
// escape to reproduce, and temp+rename already neutralizes a WRITE through a
// leaf symlink. What os.Root adds: rules must READ the existing file first (to
// preserve the developer's content); reading through a symlink that escapes the
// project is now refused, so a hostile (or simply out-of-repo) AGENTS.md symlink
// yields a recorded failure and NOTHING is written — orbeat neither follows the
// link out of the repo nor silently de-links the developer's file. Without
// os.Root this silently replaced the symlink with a standalone file.
func TestReconcileRulesLeafSymlinkRefused(t *testing.T) {
	claudeDir := t.TempDir()
	proj := t.TempDir()
	outside := t.TempDir()
	evil := filepath.Join(outside, "evil-agents.md")
	mustSymlink(t, evil, filepath.Join(proj, "AGENTS.md")) // absolute → os.Root refuses it

	res, err := ReconcileRules(claudeDir, projs(proj), []Artifact{
		{Type: "rule", Name: "std", Content: "RULE BODY"},
	}, nil)

	if err != nil {
		t.Fatalf("a symlink refusal must be non-fatal (per-unit), got %v", err)
	}
	assertNoFile(t, evil, "rule escaped the project root")
	if len(res.Failures) != 1 {
		t.Fatalf("want exactly 1 recorded failure (the escaping AGENTS.md), got %v", res.Failures)
	}
	if res.Written != 0 {
		t.Fatalf("nothing may be written when the target read escapes (Written=%d)", res.Written)
	}
}

// INVARIANT GUARD (not a red-proof). The audit's escape #1 — a dangling symlink
// exactly AT the SKILL.md target — does not reproduce: the writer's temp+rename
// replaces the symlink in place, landing the real content inside the sync root.
// This holds both before and after the os.Root change; it is pinned so a future
// switch away from atomic temp+rename can't silently reintroduce the follow.
func TestReconcileDanglingLeafSymlinkStaysContained(t *testing.T) {
	claudeDir := t.TempDir()
	outside := t.TempDir()
	evil := filepath.Join(outside, "evil.md")
	skillDir := filepath.Join(claudeDir, "skills", "esc")
	must(t, os.MkdirAll(skillDir, 0o755))
	mustSymlink(t, evil, filepath.Join(skillDir, "SKILL.md")) // dangling, absolute

	res, err := Reconcile(claudeDir, []Artifact{{Type: "skill", Name: "esc", Content: "REAL"}}, nil)
	if err != nil {
		t.Fatalf("non-fatal expected, got %v", err)
	}
	assertNoFile(t, evil, "skill escaped via a leaf symlink")
	// Content lands inside, replacing the link (the artifact wins over the link).
	if got := read(t, filepath.Join(skillDir, "SKILL.md")); got != "REAL" {
		t.Fatalf("target should hold the real artifact content, got %q", got)
	}
	_ = res
}

// TestRootedNilRootStillRefusesEscape is B1's containment proof: a *rooted
// with no underlying os.Root (openRootedPlannedAbsent, used to plan against a
// sync root that doesn't exist yet) must refuse a path escaping its boundary
// exactly as a real root would — rel's lexical check does not touch r.root,
// so its absence must not weaken the refusal. Every method that could reach
// r.root is exercised, so a future change adding a new one without routing it
// through rel first would show up here as a nil-pointer panic instead of a
// silent escape.
func TestRootedNilRootStillRefusesEscape(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "never-synced")
	var p Plan
	r := openRootedPlannedAbsent(dir, &p)
	defer r.Close()

	escaping := filepath.Join(parent, "elsewhere", "x.md")

	if _, err := r.stat(escaping); err == nil {
		t.Fatal("nil-root stat must refuse a path escaping the boundary")
	}
	if _, err := r.readFile(escaping); err == nil {
		t.Fatal("nil-root readFile must refuse a path escaping the boundary")
	}
	if err := r.writeAtomic(escaping, []byte("x"), 0o644); err == nil {
		t.Fatal("nil-root writeAtomic must refuse a path escaping the boundary")
	}
	if err := r.remove(escaping); err == nil {
		t.Fatal("nil-root remove must refuse a path escaping the boundary")
	}
	if len(p.Changes()) != 0 {
		t.Fatalf("an escape refusal must never be recorded as an intended change: %+v", p.Changes())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("nil-root operations must never create the boundary directory: %v", err)
	}
}

func TestPlanModeWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	var p Plan
	r, err := openRootedPlanned(dir, &p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.writeAtomic(filepath.Join(dir, "existing.md"), []byte("new"), 0o644); err != nil {
		t.Fatalf("planned write must not error: %v", err)
	}
	if err := r.writeAtomic(filepath.Join(dir, "sub/fresh.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("planned write must not error: %v", err)
	}
	if err := r.remove(filepath.Join(dir, "existing.md")); err != nil {
		t.Fatalf("planned remove must not error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "existing.md"))
	if err != nil || string(got) != "old" {
		t.Fatalf("plan mode wrote to disk: content=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
		t.Fatal("plan mode created a directory — writeAtomic must return before root.MkdirAll")
	}

	ch := p.Changes()
	if len(ch) != 3 {
		t.Fatalf("want 3 recorded changes, got %d: %+v", len(ch), ch)
	}
	if ch[0].Op != OpOverwrite {
		t.Errorf("writing over an existing file must record overwrite, got %q", ch[0].Op)
	}
	if ch[1].Op != OpCreate {
		t.Errorf("writing a new path must record create, got %q", ch[1].Op)
	}
	if ch[2].Op != OpRemove {
		t.Errorf("remove must record remove, got %q", ch[2].Op)
	}
}
