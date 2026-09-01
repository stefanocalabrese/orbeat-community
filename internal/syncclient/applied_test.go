package syncclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These gates all exist for one distinction: what the server SERVED versus what
// this client APPLIED. A gate whose fixture applies everything it is served
// cannot tell the two apart: a client recording the fetched slice would pass
// it. Every fixture below therefore carries at least one artifact that is served and
// deliberately does not land, and every assertion names the membership in both
// directions rather than a length.
//
// Revisions in these fixtures are deliberately distinct and never 1, so a
// recorder that writes a constant fails on the value instead of passing on the
// shape.

// wantApplied compares got against want as an exact ordered sequence. Reconcile
// sorts by ArtifactID and ReconcileRules emits in artifact-name order, so both
// are deterministic; asserting the whole sequence is what makes a spurious
// extra entry (the requested-not-applied bug) fail, which a subset check or a
// length check would not.
func wantApplied(t *testing.T, got []AppliedArtifact, want ...AppliedArtifact) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Applied = %+v, want exactly %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Applied[%d] = %+v, want %+v (full: %+v)", i, got[i], want[i], got)
		}
	}
}

// The load-bearing gate: a mixed slice where one artifact is served and NOT
// applied because the developer already owns that filename. A client that
// recorded what it fetched reports her on a revision whose bytes never touched
// her disk, which is the exact falsehood the applied-not-requested rule exists
// to prevent.
func TestReconcileAppliedExcludesUnmanagedCollision(t *testing.T) {
	dir := t.TempDir()

	// Hand-authored, unmanaged, colliding with the "mine" skill below.
	mine := filepath.Join(dir, "skills", "mine", "SKILL.md")
	must(t, os.MkdirAll(filepath.Dir(mine), 0o755))
	must(t, os.WriteFile(mine, []byte("HAND-MADE"), 0o644))

	res, err := Reconcile(dir, []Artifact{
		{ID: "id-fmt", Revision: 4, Type: "skill", Name: "fmt", Content: "S"},
		{ID: "id-rev", Revision: 7, Type: "subagent", Name: "rev", Content: "A"},
		{ID: "id-mine", Revision: 9, Type: "skill", Name: "mine", Content: "FROM-ORBEAT"},
	}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The fixture must actually be doing what it claims: without this, a
	// change that stopped skipping the collision would still let the
	// membership assertion below pass for the wrong reason.
	if len(res.Skipped) != 1 || res.Skipped[0] != "skills/mine/SKILL.md" {
		t.Fatalf("fixture broken: expected the unmanaged collision to be skipped, got %+v", res.Skipped)
	}
	if read(t, mine) != "HAND-MADE" {
		t.Fatal("fixture broken: the hand-authored file was clobbered")
	}

	wantApplied(t, res.Applied,
		AppliedArtifact{ArtifactID: "id-fmt", Revision: 4},
		AppliedArtifact{ArtifactID: "id-rev", Revision: 7},
	)
}

// A write that failed leaves the previous bytes (or nothing) on disk. Served
// says the new revision; the disk says otherwise, so Applied must not name it.
func TestReconcileAppliedExcludesFailedWrite(t *testing.T) {
	dir := t.TempDir()
	// A regular file at skills/bad makes the artifact's MkdirAll fail ENOTDIR,
	// so "bad" fails its write while "good" writes normally.
	must(t, os.MkdirAll(filepath.Join(dir, "skills"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "skills", "bad"), []byte("x"), 0o644))

	res, err := Reconcile(dir, []Artifact{
		{ID: "id-bad", Revision: 5, Type: "skill", Name: "bad", Content: "X"},
		{ID: "id-good", Revision: 8, Type: "skill", Name: "good", Content: "G"},
	}, nil)
	if err != nil {
		t.Fatalf("a per-artifact I/O failure must not be fatal: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("fixture broken: want exactly 1 write failure, got %v", res.Failures)
	}
	wantApplied(t, res.Applied, AppliedArtifact{ArtifactID: "id-good", Revision: 8})
}

// Unchanged is applied. A steady-state run writes nothing at all, and a
// recorder that only fired on a write would report an entire settled fleet as
// having nothing installed.
func TestReconcileAppliedIncludesUnchanged(t *testing.T) {
	dir := t.TempDir()
	arts := []Artifact{
		{ID: "id-fmt", Revision: 4, Type: "skill", Name: "fmt", Content: "S"},
		{ID: "id-rev", Revision: 7, Type: "subagent", Name: "rev", Content: "A"},
	}
	if _, err := Reconcile(dir, arts, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := Reconcile(dir, arts, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Added != 0 || res.Updated != 0 || res.Unchanged != 2 {
		t.Fatalf("fixture broken: want a pure steady state, got %+v", res)
	}
	wantApplied(t, res.Applied,
		AppliedArtifact{ArtifactID: "id-fmt", Revision: 4},
		AppliedArtifact{ArtifactID: "id-rev", Revision: 7},
	)
}

// The recorded revision must be the one whose bytes went to disk, not a
// constant and not the first one ever seen. The artifact keeps its id across
// the update, so only the number can distinguish a real read from a literal.
func TestReconcileAppliedRevisionFollowsTheContentThatLanded(t *testing.T) {
	dir := t.TempDir()
	if _, err := Reconcile(dir, []Artifact{
		{ID: "id-fmt", Revision: 4, Type: "skill", Name: "fmt", Content: "V1"},
	}, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := Reconcile(dir, []Artifact{
		{ID: "id-fmt", Revision: 6, Type: "skill", Name: "fmt", Content: "V2"},
	}, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Updated != 1 {
		t.Fatalf("fixture broken: want an update, got %+v", res)
	}
	if got := read(t, filepath.Join(dir, "skills", "fmt", "SKILL.md")); got != "V2" {
		t.Fatalf("fixture broken: on-disk content = %q, want V2", got)
	}
	wantApplied(t, res.Applied, AppliedArtifact{ArtifactID: "id-fmt", Revision: 6})
}

// An artifact the server never identified carries no key any deployment record
// could be stored under. It still lands on disk, which is not a skip, so the
// sibling assertion is what proves the run worked and only the unkeyed entry
// was dropped.
func TestReconcileAppliedDropsUnidentifiedArtifact(t *testing.T) {
	dir := t.TempDir()
	res, err := Reconcile(dir, []Artifact{
		{ID: "", Revision: 0, Type: "skill", Name: "old", Content: "O"},
		{ID: "id-new", Revision: 3, Type: "skill", Name: "new", Content: "N"},
	}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Added != 2 {
		t.Fatalf("fixture broken: both artifacts must land, got %+v", res)
	}
	if read(t, filepath.Join(dir, "skills", "old", "SKILL.md")) != "O" {
		t.Fatal("fixture broken: the unidentified artifact must still be written")
	}
	wantApplied(t, res.Applied, AppliedArtifact{ArtifactID: "id-new", Revision: 3})
}

// Every rule in the aggregated block lands together, so each one is named once,
// at its own revision. The skill in the slice belongs to Reconcile and must not
// leak into the rules result.
func TestReconcileRulesAppliedNamesEveryRule(t *testing.T) {
	claudeDir := t.TempDir()
	proj := t.TempDir()

	res, err := ReconcileRules(claudeDir, projs(proj), []Artifact{
		{ID: "id-b", Revision: 2, Type: "rule", Name: "b-rule", Content: "second"},
		{ID: "id-a", Revision: 5, Type: "rule", Name: "a-rule", Content: "first"},
		{ID: "id-skill", Revision: 9, Type: "skill", Name: "not-a-rule", Content: "x"},
	}, nil)
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	if res.Written != 1 {
		t.Fatalf("fixture broken: want one project written, got %+v", res)
	}
	// Emitted in artifact-name order: a-rule before b-rule.
	wantApplied(t, res.Applied,
		AppliedArtifact{ArtifactID: "id-a", Revision: 5},
		AppliedArtifact{ArtifactID: "id-b", Revision: 2},
	)
}

// A developer with no registered project is entitled to rules that reach
// nothing. There is no failure and no warning anywhere in the result, so
// nothing but Applied can express it. The second half runs the SAME artifacts
// against a real project: without it, a recorder that never records anything at
// all would pass the first half.
func TestReconcileRulesAppliedEmptyWithoutProjects(t *testing.T) {
	claudeDir := t.TempDir()
	arts := []Artifact{{ID: "id-a", Revision: 5, Type: "rule", Name: "a-rule", Content: "first"}}

	res, err := ReconcileRules(claudeDir, nil, arts, nil)
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	if len(res.Failures) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("fixture broken: a rule with nowhere to go is silent, got %+v", res)
	}
	wantApplied(t, res.Applied)

	proj := t.TempDir()
	res2, err := ReconcileRules(claudeDir, projs(proj), arts, nil)
	if err != nil {
		t.Fatalf("rules with a project: %v", err)
	}
	wantApplied(t, res2.Applied, AppliedArtifact{ArtifactID: "id-a", Revision: 5})
}

// A project whose AGENTS.md carries a malformed ORBEAT-RULES marker is skipped
// rather than spliced, so the rules never land there, while its CLAUDE.md
// import still merges and counts as Written. Written is therefore not the
// applied signal, and a recorder keyed on it reports rules the developer's
// AGENTS.md does not contain. The second half adds a healthy project to prove
// "at least one project" really is the rule.
func TestReconcileRulesAppliedExcludesMalformedMarkerProject(t *testing.T) {
	claudeDir := t.TempDir()
	broken := t.TempDir()
	agents := filepath.Join(broken, "AGENTS.md")
	orphan := "<!-- ORBEAT-RULES:BEGIN sha=deadbeef0000 x -->\n"
	must(t, os.WriteFile(agents, []byte(orphan+"# dev notes\n"), 0o644))

	arts := []Artifact{{ID: "id-a", Revision: 5, Type: "rule", Name: "a-rule", Content: "first"}}
	res, err := ReconcileRules(claudeDir, projs(broken), arts, nil)
	if err != nil {
		t.Fatalf("rules: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "malformed") {
		t.Fatalf("fixture broken: want one malformed-marker warning, got %+v", res.Warnings)
	}
	if res.Written != 1 {
		t.Fatalf("fixture broken: the CLAUDE.md import must still have been written, got %+v", res)
	}
	if strings.Contains(read(t, agents), "## a-rule") {
		t.Fatal("fixture broken: the malformed AGENTS.md must not have been spliced")
	}
	wantApplied(t, res.Applied)

	healthy := t.TempDir()
	res2, err := ReconcileRules(claudeDir, projs(broken, healthy), arts, nil)
	if err != nil {
		t.Fatalf("rules with a healthy project: %v", err)
	}
	wantApplied(t, res2.Applied, AppliedArtifact{ArtifactID: "id-a", Revision: 5})
}

// Seed memory rides its subagent artifact and never asserts a deployment of its
// own. This is the path that decides it: the agent file collided with a
// hand-authored one and was NOT applied, while the seed block merged perfectly.
// A seed-sourced record would claim a revision whose agent body is not on disk.
func TestSeededSubagentCollisionIsNotApplied(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, "agents", "rev.md")
	must(t, os.MkdirAll(filepath.Dir(mine), 0o755))
	must(t, os.WriteFile(mine, []byte("HAND-MADE"), 0o644))

	art := Artifact{
		ID: "id-rev", Revision: 7, Type: "subagent", Name: "rev", Content: "FROM-ORBEAT",
		MemoryScope: "user", MemorySeed: "seed body",
	}

	res, err := Reconcile(dir, []Artifact{art}, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("fixture broken: want the agent file skipped, got %+v", res)
	}
	wantApplied(t, res.Applied)

	seedRes, err := ReconcileSeeds(dir, nil, []Artifact{art}, nil)
	if err != nil {
		t.Fatalf("seeds: %v", err)
	}
	if seedRes.Written != 1 {
		t.Fatalf("fixture broken: the seed must still have landed, got %+v", seedRes)
	}
	if read(t, mine) != "HAND-MADE" {
		t.Fatal("fixture broken: the hand-authored agent file was clobbered")
	}
}
