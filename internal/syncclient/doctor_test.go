package syncclient

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDiagnoseReportsAnAbsentSyncRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), "never-synced")
	rep := Diagnose(home, nil, "", "", "")

	// 2, not 1: Diagnose always appends a CheckAuth note last (see
	// TestDiagnoseOnAHealthyTreeHasOnlyTheAuthNote below), so Findings[0] is
	// still the one finding this test actually cares about — the auth note is
	// checked separately, everywhere, and re-asserting it in every other test
	// would just be the same assertion copy-pasted N times.
	if len(rep.Findings) != 2 {
		t.Fatalf("want exactly 2 findings (CheckSyncRoot + the always-present auth note) for an absent sync root, got %+v", rep.Findings)
	}
	f := rep.Findings[0]
	if f.Check != CheckSyncRoot {
		t.Errorf("want CheckSyncRoot, got %q", f.Check)
	}
	if f.Path != home {
		t.Errorf("finding must name the path it concerns: got %q want %q", f.Path, home)
	}
	if f.Remedy == "" {
		t.Error("every finding must name what resolves it — an operator reading this has to know what to do")
	}
}

// TestDiagnoseOnAHealthyTreeHasOnlyTheAuthNote replaces what used to be
// "silent on a healthy tree": doctor now always emits exactly one
// SeverityNote (CheckAuth) deferring to 'orbeat-sync status', on every tree,
// healthy or not — see checkAuth's doc comment. "Zero findings" is no longer
// the right expectation for a healthy tree; it never will be again. What must
// still hold is zero PROBLEMS — Report.Problems() unchanged is exactly what
// scripts/smoke.sh's D1 gate's `.problems == 0` assertion on a healthy tree
// depends on, and what stops this note from turning a clean tree into one
// that reads as broken.
func TestDiagnoseOnAHealthyTreeHasOnlyTheAuthNote(t *testing.T) {
	home := t.TempDir()
	rep := Diagnose(home, nil, "", "", "")

	if rep.Problems() != 0 {
		t.Fatalf("a healthy tree must report zero problems, got %d: %+v", rep.Problems(), rep.Findings)
	}
	if len(rep.Findings) != 1 {
		t.Fatalf("want exactly 1 finding (the always-present auth note) on a healthy tree, got %+v", rep.Findings)
	}
	if f := rep.Findings[0]; f.Check != CheckAuth {
		t.Errorf("want CheckAuth, got %q", f.Check)
	} else if f.Severity != SeverityNote {
		t.Errorf("the auth note must never be a problem — it fires on every tree, healthy or not; got severity %q", f.Severity)
	} else if f.Remedy == "" {
		t.Error("every finding must name what resolves it")
	}
}

// TestDiagnoseOnAnUnreadableSyncRootBlamesTheRootNotTheManifest is B3: os.Stat
// SUCCEEDS on a mode-000 directory (statting a directory needs search
// permission on its PARENT, not on the directory itself), so the
// "unreadable sync root" branch checkSyncRoot's docs promise never fired —
// and checkManifest then blamed the perfectly intact manifest instead,
// because loadManifest's error is really "can't open the sync root", not
// "can't parse the manifest". An operator who follows THAT remedy (remove the
// manifest and re-sync) deletes the ledger, orphaning every managed block on
// the machine.
func TestDiagnoseOnAnUnreadableSyncRootBlamesTheRootNotTheManifest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced, this test would pass without exercising anything")
	}

	home := t.TempDir()
	// The manifest is perfectly intact — the defect is that an unrelated fault
	// (an unreadable root) got misreported as a manifest problem, so a healthy
	// manifest here is the point, not an accident.
	must(t, saveManifest(home, manifest{}, nil))

	if err := os.Chmod(home, 0o000); err != nil {
		t.Fatal(err)
	}
	// t.TempDir's own cleanup fails to remove a mode-000 directory — restore
	// the mode ourselves or this test poisons every test that runs after it.
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })

	// Confirm the permission bit is actually enforced on this filesystem before
	// trusting anything below — some environments (containers, certain
	// filesystems) don't enforce it, and a permission test that passes without
	// the permission being enforced reports coverage that does not exist.
	if _, err := os.ReadDir(home); err == nil {
		t.Skip("mode 000 did not block os.ReadDir here — permissions not enforced, skipping")
	}

	rep := Diagnose(home, nil, "", "", "")

	// 2, not 1: the always-present auth note (see
	// TestDiagnoseOnAHealthyTreeHasOnlyTheAuthNote) is always Findings' last
	// entry, so index 0 below is still the finding this test cares about.
	if len(rep.Findings) != 2 {
		t.Fatalf("want exactly 2 findings (CheckSyncRoot + the always-present auth note) for an unreadable sync root, got %+v", rep.Findings)
	}
	if f := rep.Findings[0]; f.Check != CheckSyncRoot {
		t.Errorf("want CheckSyncRoot, got %q (%+v)", f.Check, f)
	}
}

// TestDiagnoseOnASyncRootThatIsARegularFileBlamesTheRootNotTheManifest covers
// B3's other trigger: ~/.claude existing as a plain file. loadManifest fails
// to open it as a root (ENOTDIR, not ErrNotExist), so the pre-fix code
// reported a SECOND finding naming "<file>/.orbeat-sync-manifest.json" — a
// path that cannot exist, since claudeDir is not a directory at all.
func TestDiagnoseOnASyncRootThatIsARegularFileBlamesTheRootNotTheManifest(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, ".claude")
	if err := os.WriteFile(home, []byte("oops, not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := Diagnose(home, nil, "", "", "")

	// 2, not 1: same always-present auth note as above, always last.
	if len(rep.Findings) != 2 {
		t.Fatalf("want exactly 2 findings (CheckSyncRoot + the always-present auth note) for a sync root that is a regular file, got %+v", rep.Findings)
	}
	if f := rep.Findings[0]; f.Check != CheckSyncRoot {
		t.Errorf("want CheckSyncRoot, got %q (%+v)", f.Check, f)
	}
}

func TestDiagnoseReportsAnUnreachableProject(t *testing.T) {
	home := t.TempDir()
	gone := filepath.Join(t.TempDir(), "unmounted")

	rep := Diagnose(home, []string{gone}, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckProject {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckProject finding, got %+v", rep.Findings)
	}
	if found.Path != gone {
		t.Errorf("finding must name the unreachable path, got %q", found.Path)
	}
}

// TestDiagnoseDedupesDuplicateProjectEntries is doctor-fixes §4 item 7:
// AddProject already refuses to register the same path twice, so only a
// hand-edited projects.json can list one path twice — but when it does, every
// project-scoped check reported the identical finding twice, which reads as
// two distinct problems rather than one. projects.json is loaded by
// LoadProjects (projects.go, out of scope for this slice) and handed to
// Diagnose as a plain []string, so the same literal path appears twice here
// exactly as a hand-edited file would produce it.
func TestDiagnoseDedupesDuplicateProjectEntries(t *testing.T) {
	home := t.TempDir()
	gone := filepath.Join(t.TempDir(), "unmounted")

	rep := Diagnose(home, []string{gone, gone}, "", "", "")

	var n int
	for _, f := range rep.Findings {
		if f.Check == CheckProject && f.Path == gone {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 CheckProject finding for a duplicated project entry, got %d: %+v", n, rep.Findings)
	}
}

func TestDiagnoseReportsPreservedEntriesAsNotesNotProblems(t *testing.T) {
	home := t.TempDir()
	gone := filepath.Join(t.TempDir(), "unmounted")
	// The ledger holds an entry for a project that is not currently reachable —
	// v1.15.0 preserves these DELIBERATELY so a remounted volume self-heals.
	must(t, saveManifest(home, manifest{Rules: []string{gone}}, nil))

	// Deliberately NOT passed as a registered project (unlike the fixture
	// below): if gone were also registered, checkProjects would ALSO report
	// it as a CheckProject problem, and defect B's fix suppresses the
	// preserved-entry note whenever that happens — which would make this
	// severity assertion pass vacuously (or rather fail to find a finding at
	// all) for a reason that has nothing to do with what this test pins. A
	// ledger entry for a project that was simply never (or no longer)
	// registered must still surface as a note on its own.
	rep := Diagnose(home, nil, "", "", "")

	for _, f := range rep.Findings {
		if f.Check == CheckPreservedEntry {
			if f.Severity != SeverityNote {
				t.Errorf("a preserved entry is correct behaviour, not a problem — got severity %q; "+
					"reporting it as a problem sends users to delete state that is protecting them", f.Severity)
			}
			return
		}
	}
	t.Fatalf("want a CheckPreservedEntry finding, got %+v", rep.Findings)
}

// TestDiagnoseSuppressesPreservedEntryNoteWhenProjectAlreadyReportedUnreachable
// is defect B: a REGISTERED project that is unreachable produces two findings
// for the identical path — CheckProject's "reconnect the volume or fix the
// path ... or run 'orbeat-sync project remove'" PROBLEM, and
// CheckPreservedEntry's "no action needed" NOTE. Both are individually true;
// read together in the same report they cancel out and the operator learns
// nothing. This is the exact fixture the OLD
// TestDiagnoseReportsPreservedEntriesAsNotesNotProblems used — moved here
// because that is what it actually exercises now.
func TestDiagnoseSuppressesPreservedEntryNoteWhenProjectAlreadyReportedUnreachable(t *testing.T) {
	home := t.TempDir()
	gone := filepath.Join(t.TempDir(), "unmounted")
	must(t, saveManifest(home, manifest{Rules: []string{gone}}, nil))

	rep := Diagnose(home, []string{gone}, "", "", "")

	var problem *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckProject && rep.Findings[i].Path == gone {
			problem = &rep.Findings[i]
		}
	}
	if problem == nil {
		t.Fatalf("want a CheckProject finding for %s, got %+v", gone, rep.Findings)
	}
	for _, f := range rep.Findings {
		if f.Check == CheckPreservedEntry {
			t.Errorf("a preserved-entry note for a path already reported as a CheckProject "+
				"problem contradicts it (\"fix this\" next to \"no action needed\") — got %+v", f)
		}
	}
}

// TestCheckLedgerDriftUsesThePassedManifestNotADiskRead is doctor-fixes §4
// item 3: Diagnose used to have checkPreservedEntries, checkLedgerDrift,
// checkOrphanedBlocks and checkMarkers each call loadManifest independently,
// so a `sync` running concurrently could swap the manifest file (atomic
// temp+rename) between two of those reads — drift judged against one
// version, orphans against another.
//
// The fix reads the manifest exactly once in Diagnose and threads the result
// into every check via its own signature. This test proves that directly: it
// calls checkLedgerDrift with a manifest that deliberately DISAGREES with
// what's actually saved on disk, and asserts the finding follows the PASSED
// value, not a fresh read. This is deliberately NOT just a compile-time
// signature check — it fails the same way if checkLedgerDrift's body were
// changed back to re-read loadManifest(claudeDir) and discard its arguments,
// which is the exact regression "read once" exists to prevent; see the
// red-proof in doctor.go's history for that mutant applied and reverted.
//
// The pre-fix checkLedgerDrift took no manifest argument at all
// (checkLedgerDrift(claudeDir string)), so a four-independent-reads
// implementation cannot even be called this way — this test would not
// compile against it, which is itself part of the proof: writing a
// meaningful test for "read once" was only possible by making the single
// read visible in the signature, per the brief's own suggested approach.
func TestCheckLedgerDriftUsesThePassedManifestNotADiskRead(t *testing.T) {
	home := t.TempDir()
	// On disk: no Files entry at all — a fresh read would find no drift.
	must(t, saveManifest(home, manifest{}, nil))

	// Passed in: a Files entry naming a file that does not exist on disk. The
	// path has to be one orbeat-sync could actually have written
	// (validManagedFilePath), or the drift check never runs: a shape-invalid
	// entry gets its own note and is skipped, since sync drops it rather than
	// recreating it.
	passed := manifest{Files: []string{"agents/ghost.md"}}

	var r Report
	// nil projects: this test is about the Files ledger, which does not consult
	// the trusted-root set at all.
	r.checkLedgerDrift(home, nil, passed, nil)

	want := filepath.Join(home, "agents", "ghost.md")
	var found bool
	for _, f := range r.Findings {
		if f.Check == CheckLedgerDrift && f.Path == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("checkLedgerDrift did not use the manifest it was passed — it must not "+
			"re-read the manifest from disk, got %+v", r.Findings)
	}
}

func TestDiagnoseReportsLedgerDrift(t *testing.T) {
	home := t.TempDir()
	// The manifest tracks a managed file the disk no longer has — something
	// deleted it out of band. The next sync recreates it (that part is fine),
	// but the operator cannot see that coming without doctor telling them.
	must(t, saveManifest(home, manifest{Files: []string{"agents/ghost.md"}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckLedgerDrift {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckLedgerDrift finding, got %+v", rep.Findings)
	}
	want := filepath.Join(home, "agents", "ghost.md")
	if found.Path != want {
		t.Errorf("finding must name the missing file, got %q want %q", found.Path, want)
	}
	if found.Severity != SeverityProblem {
		t.Errorf("out-of-band deletion needs attention — got severity %q", found.Severity)
	}
	if found.Remedy == "" {
		t.Error("every finding must name what resolves it")
	}
}

func TestDiagnoseIsSilentOnAnIntactLedger(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "agents", "a.md"), []byte("BODY"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(t, saveManifest(home, manifest{Files: []string{"agents/a.md"}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	for _, f := range rep.Findings {
		if f.Check == CheckLedgerDrift {
			t.Fatalf("a ledger entry backed by a real file must not be reported, got %+v", f)
		}
	}
}

// TestDiagnoseReportsAVanishedSeedFileAsANote is defect C: a ledgered seed
// MEMORY.md that no longer exists on disk currently produces NOTHING —
// checkLedgerDrift only ever walked the manifest's Files ledger, never Seeds.
// A MEMORY.md lives inside a developer's own project (unlike a rendered
// skill/subagent Files entry, which is wholly orbeat's content), so deleting
// it is a legitimate, ordinary developer action, and the ledger self-corrects
// on the next sync either way (ReconcileSeeds' write pass recreates it if the
// subagent is still entitled; its strip pass treats a missing file as
// "nothing to strip" and the entry just drops) — so this must surface as a
// NOTE, not a problem: calling it a problem would be the same class of false
// positive defect A fixes.
//
// The project root itself stays reachable (only the seed file is gone), so
// this is distinguishable from checkPreservedEntries' unreachable-PROJECT
// case, which is a different fault with a different remedy.
func TestDiagnoseReportsAVanishedSeedFileAsANote(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	path := filepath.Join(proj, ".claude", "agent-memory", "gone-agent", "MEMORY.md")
	must(t, saveManifest(home, manifest{Seeds: map[string][]string{"gone-agent": {path}}}, nil))

	rep := Diagnose(home, []string{proj}, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckLedgerDrift && rep.Findings[i].Path == path {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckLedgerDrift finding for the vanished seed %s, got %+v", path, rep.Findings)
	}
	if found.Severity != SeverityNote {
		t.Errorf("a developer may legitimately delete their own MEMORY.md, and the ledger "+
			"self-corrects either way — got severity %q, want a note", found.Severity)
	}
	if found.Remedy == "" {
		t.Error("every finding must name what resolves it")
	}
}

func TestDiagnoseReportsAMalformedRulesMarker(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	// Constructed literally, not via renderRulesBlock/mergeRules: an orphan
	// BEGIN with no matching END is exactly the state rulesMarkersHealthy
	// exists to catch, and calling the package's own block-builder here would
	// test that helper instead of this check.
	orphan := "some dev content\n\n" +
		"<!-- ORBEAT-RULES:BEGIN sha=0123456789ab — managed by orbeat-sync; edit OUTSIDE this block -->\n" +
		"stale body\n"
	if err := os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte(orphan), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := Diagnose(home, []string{proj}, "", "", "")

	var found *Finding
	agentsPath := filepath.Join(proj, "AGENTS.md")
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckMarkers && rep.Findings[i].Path == agentsPath {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckMarkers finding for %s, got %+v", agentsPath, rep.Findings)
	}
	if found.Severity != SeverityProblem {
		t.Errorf("a malformed marker needs attention — got severity %q", found.Severity)
	}
	if !strings.Contains(found.Remedy, "keep skipping") || !strings.Contains(found.Remedy, "by hand") {
		t.Errorf("remedy must say sync will keep skipping the file until it is repaired by hand, got %q", found.Remedy)
	}
}

func TestDiagnoseIsSilentOnHealthyMarkers(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("plain dev content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := Diagnose(home, []string{proj}, "", "", "")

	for _, f := range rep.Findings {
		if f.Check == CheckMarkers {
			t.Fatalf("a file with no markers at all must not be reported, got %+v", f)
		}
	}
}

// TestDiagnoseDoesNotCallDocumentationOfTheMarkerAnOrphan is defect A: an
// AGENTS.md that merely DOCUMENTS the ORBEAT-RULES marker syntax in prose —
// a team README-style line explaining what the delimiters look like, not an
// actual managed block — still matches rulesBeginRe/rulesEndRe, which are
// bare substring regexes with no requirement that a real block follow. The
// old orphan check judged presence on the BEGIN-marker regex alone, so it
// reported this prose as an orphaned block with a "strip it by hand" remedy —
// an operator following that advice deletes their own documentation, since
// there is no block there to strip.
//
// The prose is written out literally here, not generated via
// renderRulesBlock or any of the package's own regexes/helpers: building the
// fixture from the same regex the code uses would test the regex against
// itself, not the real defect.
func TestDiagnoseDoesNotCallDocumentationOfTheMarkerAnOrphan(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	doc := "# Team conventions\n\n" +
		"orbeat-sync manages a block in this file, delimited by\n" +
		"`<!-- ORBEAT-RULES:BEGIN -->` and `<!-- ORBEAT-RULES:END -->`.\n" +
		"Do not hand-edit anything between those two lines.\n"
	agentsPath := filepath.Join(proj, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := Diagnose(home, []string{proj}, "", "", "")

	var sawMarkers bool
	for _, f := range rep.Findings {
		if f.Check == CheckOrphanedBlock && f.Path == agentsPath {
			t.Fatalf("prose ABOUT the marker syntax is not a block — deleting it as advised "+
				"would destroy the operator's own documentation: %+v", f)
		}
		if f.Check == CheckMarkers && f.Path == agentsPath {
			sawMarkers = true
		}
	}
	// The companion CheckMarkers finding for this same file is TRUE — sync
	// really does refuse to splice a file whose markers don't come in
	// balanced pairs — so the operator is still told something accurate and
	// actionable about it. Narrowing the orphan check loses no real signal.
	if !sawMarkers {
		t.Fatalf("want a CheckMarkers finding for %s (sync still refuses to splice this file), got %+v",
			agentsPath, rep.Findings)
	}
}

// TestDiagnoseDoesNotReadAnOversizedFileWhole is doctor-fixes §4 item 5: a
// managed file doctor scans (AGENTS.md/CLAUDE.md for rules, a ledgered
// MEMORY.md for seeds) is read with os.ReadFile — unbounded, in memory, in
// full — to look for a marker. A huge file at one of those paths burns
// memory proportional to its size just to answer a yes/no question about a
// few bytes near the start.
//
// The fixture pads AGENTS.md past the cap and appends an ORPHAN ORBEAT-RULES
// BEGIN marker with no matching END — content that WOULD trip
// rulesMarkersHealthy's "malformed marker" problem finding if read in full.
// If doctor still reports that malformed-marker problem, it read the whole
// file despite the cap; the fix must instead leave the marker unscanned and
// say so, never claim to have inspected content past the cap.
func TestDiagnoseDoesNotReadAnOversizedFileWhole(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	// A literal, not the doctor.go cap constant: this test must still compile
	// (and show a genuine RUNTIME failure, not a build error) before that
	// constant exists. 1 MiB + 1 mirrors the v1.18.0 audit's read-cap style
	// used elsewhere in this repo (auth.maxDiscoveryBodyBytes,
	// govern.llmMaxRespBytes, api.maxRequestBodyBytes are all 1<<20).
	padding := strings.Repeat("x", 1<<20+1)
	orphan := padding +
		"\n<!-- ORBEAT-RULES:BEGIN sha=0123456789ab — managed by orbeat-sync; edit OUTSIDE this block -->\n" +
		"stale body\n"
	agentsPath := filepath.Join(proj, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte(orphan), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := Diagnose(home, []string{proj}, "", "", "")

	var sawTooLarge, sawMalformed bool
	for _, f := range rep.Findings {
		if f.Path != agentsPath {
			continue
		}
		switch {
		case f.Check == CheckMarkers && f.Severity == SeverityNote:
			sawTooLarge = true
		case f.Check == CheckMarkers && f.Severity == SeverityProblem:
			sawMalformed = true
		}
	}
	if sawMalformed {
		t.Fatalf("doctor reported a malformed-marker PROBLEM for a file past the read cap — "+
			"it must have read the whole file despite the size limit: %+v", rep.Findings)
	}
	if !sawTooLarge {
		t.Fatalf("want a note saying the oversized file was not scanned, got %+v", rep.Findings)
	}
}

// TestReadCappedRefusesANonRegularFile is the other half of doctor-fixes §4
// item 5, and the half that actually matters: a byte cap on the READ does
// nothing when os.Open itself is what blocks. A FIFO with no reader/writer
// attached hangs any os.Open(O_RDONLY) against it indefinitely — no amount of
// io.LimitReader downstream can rescue a call that never returns. readCapped
// must refuse a non-regular file on the os.Stat, before ever calling Open.
//
// Tests the helper directly rather than driving Diagnose: if this refusal
// ever regresses, a helper-level test fails fast (or times out fast, see the
// -timeout note on this test's red-proof); a Diagnose-level test would hang
// until Go's test binary timeout and get reported as a suite-wide timeout,
// not attributed to this defect.
//
// syscall.Mkfifo is unix-only. This repo's actual targets — macOS (dev) and
// ubuntu-latest (CI, see .github/workflows/ci.yml) — are both unix, so no
// build tag is needed; t.Skipf is enough defensive belt-and-braces for a
// platform that can't make a FIFO, matching this file's existing idiom for
// platform-dependent guards (TestDiagnoseOnAnUnreadableSyncRootBlamesTheRootNotTheManifest
// and TestDiagnoseReportsAnUnreadableSeedMarkerFile both t.Skip rather than
// build-tag when a permission/filesystem behavior isn't testable here).
func TestReadCappedRefusesANonRegularFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	// No reader/writer is attached, so os.Open on this path BLOCKS
	// indefinitely. readCapped must refuse it on the os.Stat, before ever
	// calling Open — a byte cap cannot save a call that never returns.
	if _, _, err := readCapped(p); err == nil {
		t.Fatal("readCapped accepted a FIFO — os.Open on it blocks forever, and no read cap can rescue a call that never returns")
	}
}

func TestDiagnoseReportsAnUnparseableManifest(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, manifestName), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckManifest {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckManifest finding for an unparseable manifest, got %+v", rep.Findings)
	}
	if found.Severity != SeverityProblem {
		t.Errorf("a manifest sync cannot trust needs attention — got severity %q", found.Severity)
	}
	if found.Path != filepath.Join(home, manifestName) {
		t.Errorf("finding must name the manifest path, got %q", found.Path)
	}
}

// TestDiagnoseIsSilentOnAMissingManifest is the pair that stops
// TestDiagnoseReportsAnUnparseableManifest's check from firing on every
// first-ever run: a machine that has never synced has no manifest at all,
// and that must NOT be reported as a problem.
func TestDiagnoseIsSilentOnAMissingManifest(t *testing.T) {
	home := t.TempDir() // sync root exists; no manifest file inside it

	rep := Diagnose(home, nil, "", "", "")

	for _, f := range rep.Findings {
		if f.Check == CheckManifest {
			t.Fatalf("a missing manifest is not a problem — first-run doctor would report a phantom problem, got %+v", f)
		}
	}
}

// TestDiagnoseReportsATraversingFilesEntryAsAManifestProblem is the doctor
// half of the "Clean while hard-down" bug: resolveContained's traversal
// guard on a Files ledger entry is the exact fatalError Reconcile returns
// for the SAME entry, aborting every 'orbeat-sync sync' at exit 2 before
// seeds or rules ever run. Silently discarding that error here — "not this
// check's concern" — left doctor reporting Clean on a hard-down product,
// with no other check picking it up either.
func TestDiagnoseReportsATraversingFilesEntryAsAManifestProblem(t *testing.T) {
	home := t.TempDir()
	must(t, saveManifest(home, manifest{Files: []string{"../../../../etc/hosts"}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckManifest {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckManifest finding for a traversing files ledger entry, got %+v", rep.Findings)
	}
	if found.Severity != SeverityProblem {
		t.Errorf("a traversing entry aborts every sync at exit 2 — got severity %q", found.Severity)
	}
	if !strings.Contains(found.Detail, "../../../../etc/hosts") {
		t.Errorf("detail must name the offending entry, got %q", found.Detail)
	}
	if !strings.Contains(found.Detail, "exit 2") {
		t.Errorf("detail must say plainly that every sync aborts at exit 2 until the entry is repaired, got %q", found.Detail)
	}
	if found.Remedy == "" {
		t.Error("every finding must name what resolves it")
	}
}

// TestDiagnoseReportsAShapeInvalidRulesEntryAsANote covers the sibling site
// in checkPreservedEntries' Rules loop: unlike the Files traversal above,
// ReconcileRules' own strip pass just warns ("ignoring malformed rules
// ledger entry") and drops the entry on its own next run — non-fatal, so
// this is a note, not a problem.
func TestDiagnoseReportsAShapeInvalidRulesEntryAsANote(t *testing.T) {
	home := t.TempDir()
	must(t, saveManifest(home, manifest{Rules: []string{"not-an-absolute-path"}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckManifest && strings.Contains(rep.Findings[i].Detail, "not-an-absolute-path") {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckManifest finding for a shape-invalid rules ledger entry, got %+v", rep.Findings)
	}
	if found.Severity != SeverityNote {
		t.Errorf("a shape-invalid rules entry only warns and self-heals — got severity %q", found.Severity)
	}
	if found.Remedy == "" {
		t.Error("every finding must name what resolves it")
	}
}

// TestDiagnoseReportsAShapeInvalidSeedEntryAsANoteInPreservedEntries covers
// the first of the two validSeedPath sites: checkPreservedEntries' Seeds
// loop, which normally guesses a project root via seedProjectGuess to check
// reachability — a shape-invalid path can't safely go through that
// derivation, so this site reports what it could not verify.
func TestDiagnoseReportsAShapeInvalidSeedEntryAsANoteInPreservedEntries(t *testing.T) {
	home := t.TempDir()
	must(t, saveManifest(home, manifest{Seeds: map[string][]string{"agent-x": {"relative/MEMORY.md"}}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckManifest && strings.Contains(rep.Findings[i].Detail, "reachable") {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckManifest note from checkPreservedEntries for a shape-invalid seed ledger entry, got %+v", rep.Findings)
	}
	if found.Severity != SeverityNote {
		t.Errorf("a shape-invalid seed entry only warns and self-heals — got severity %q", found.Severity)
	}
	if !strings.Contains(found.Detail, "relative/MEMORY.md") {
		t.Errorf("detail must name the offending entry, got %q", found.Detail)
	}
}

// TestDiagnoseReportsAShapeInvalidSeedEntryAsANoteInMarkers covers the
// second validSeedPath site: checkMarkers' Seeds loop, which normally reads
// the file at the ledger path to check its marker health — a shape-invalid
// path fails the same defense-in-depth guard the reconcilers apply before
// touching untrusted ledger input, so doctor must not read it either.
func TestDiagnoseReportsAShapeInvalidSeedEntryAsANoteInMarkers(t *testing.T) {
	home := t.TempDir()
	must(t, saveManifest(home, manifest{Seeds: map[string][]string{"agent-x": {"relative/MEMORY.md"}}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckManifest && strings.Contains(rep.Findings[i].Detail, "will not read it") {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckManifest note from checkMarkers for a shape-invalid seed ledger entry, got %+v", rep.Findings)
	}
	if found.Severity != SeverityNote {
		t.Errorf("a shape-invalid seed entry only warns and self-heals — got severity %q", found.Severity)
	}
}

// writeOrphanSeedCandidate writes a well-formed ORBEAT-SEED block for name at
// the exact path scanSeedFiles/ReconcileSeeds expect under root
// (<root>/<name>/MEMORY.md), and returns that path. Shared by the orphan pair
// below so the only difference between them is the ledger, not the fixture.
func writeOrphanSeedCandidate(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(path, []byte(renderSeedBlock(name, "some agent memory")), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDiagnoseFindsAnOrphanedBlock is the check with teeth: a seed MEMORY.md
// carries a well-formed managed block, but the ledger is empty. This test owns
// only that the check FIRES and names a remedy; its SEVERITY is owned by
// TestDiagnoseCallsAnOrphanedSeedBlockANoteBecauseTheNextSyncStripsIt, which
// also runs the sync that proves the severity right. This test used to assert
// SeverityProblem and Problems() == 1, and both were assertions on a bug: the
// seed strip pass is not ledger-only, so it removes this block by itself. The
// Report.Problems() pin those lines carried moved to
// TestDiagnoseFindsOrphanedRulesBlocksInBothManagedFiles, where a real
// SeverityProblem still exists to count.
func TestDiagnoseFindsAnOrphanedBlock(t *testing.T) {
	home := t.TempDir()
	path := writeOrphanSeedCandidate(t, filepath.Join(home, "agent-memory"), "orphan-agent")

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckOrphanedBlock && rep.Findings[i].Path == path {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckOrphanedBlock finding for %s, got %+v", path, rep.Findings)
	}
	if found.Remedy == "" {
		t.Error("every finding must name what resolves it")
	}
}

// TestDiagnoseDoesNotCallALedgeredBlockAnOrphan is the pair that stops a
// check reporting every scanned block as an orphan — which would pass the
// test above on its own. The IDENTICAL file, now listed in the manifest's
// Seeds ledger: nothing about the on-disk content differs.
func TestDiagnoseDoesNotCallALedgeredBlockAnOrphan(t *testing.T) {
	home := t.TempDir()
	path := writeOrphanSeedCandidate(t, filepath.Join(home, "agent-memory"), "orphan-agent")
	must(t, saveManifest(home, manifest{Seeds: map[string][]string{"orphan-agent": {path}}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	for _, f := range rep.Findings {
		if f.Check == CheckOrphanedBlock {
			t.Fatalf("a block the ledger lists must not be reported as an orphan, got %+v", f)
		}
	}
}

// TestDiagnoseFindsOrphanedRulesBlocksInBothManagedFiles targets
// checkOrphanedRulesFile directly: a registered project carries a
// well-formed ORBEAT-RULES block in BOTH of its managed files (AGENTS.md and
// CLAUDE.md), and the ledger has no Rules entry for it at all. Unlike the
// seed orphan scan (one candidate list, one loop), checkOrphanedRulesFile is
// called once per managed file with the SAME project root — so a mutant that
// makes it never add a finding drops both, and a mutant that only checks one
// of the two files would still pass a single-file fixture. Two files, two
// independent findings, is the fixture that pins the per-file call.
func TestDiagnoseFindsOrphanedRulesBlocksInBothManagedFiles(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	block := renderRulesBlock("some governed rule content")
	agentsPath := filepath.Join(proj, "AGENTS.md")
	claudePath := filepath.Join(proj, "CLAUDE.md")
	if err := os.WriteFile(agentsPath, []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	// No manifest at all: an absent manifest is the same "nothing ledgered"
	// state as an empty Rules slice (loadManifest returns a zero-value
	// manifest), and exercising that path here doubles as coverage that
	// checkOrphanedBlocks doesn't require a manifest file to exist.

	rep := Diagnose(home, []string{proj}, "", "", "")

	found := map[string]bool{}
	for _, f := range rep.Findings {
		if f.Check == CheckOrphanedBlock && (f.Path == agentsPath || f.Path == claudePath) {
			found[f.Path] = true
			if f.Severity != SeverityProblem {
				t.Errorf("an orphaned rules block needs attention — got severity %q for %q", f.Severity, f.Path)
			}
			if f.Remedy == "" {
				t.Errorf("every finding must name what resolves it (%q)", f.Path)
			}
		}
	}
	if !found[agentsPath] {
		t.Errorf("want a CheckOrphanedBlock finding for %s, got %+v", agentsPath, rep.Findings)
	}
	if !found[claudePath] {
		t.Errorf("want a CheckOrphanedBlock finding for %s, got %+v", claudePath, rep.Findings)
	}
	// Two SeverityProblem findings and one CheckAuth note, so Problems() must
	// report 2. This pins Report.Problems() itself, not just the per-check
	// finding, and it lives here because the rules half is the half that
	// legitimately produces a problem (the seed half is a note: see
	// TestDiagnoseCallsAnOrphanedSeedBlockANoteBecauseTheNextSyncStripsIt).
	// A Problems() that always returned 0, or that counted every finding
	// regardless of severity, satisfies every other assertion in this file,
	// because none of them call Problems() directly: the human renderer's
	// trailing summary line and the --json "problems" field both derive from
	// it, so a wrong count propagates silently everywhere that reads it,
	// right down to scripts/smoke.sh's D1 gate.
	if got := rep.Problems(); got != 2 {
		t.Errorf("Problems() = %d, want 2 (two SeverityProblem findings, one note)", got)
	}
}

// TestDiagnoseReportsAMalformedMarkerOnALedgeredSeed targets
// checkSeedMarkerFile via checkMarkers' Seeds-ledger walk — DISTINCT from
// checkOrphanedSeedsUnder's fs-scan path (TestDiagnoseFindsAnOrphanedBlock's
// sibling), which only ever runs for a file the ledger does NOT list. Here
// the seed IS ledgered, so the orphan scan skips it entirely (ledgered ==
// true) and the only code path that can ever flag its malformed marker is
// checkMarkers walking m.Seeds and calling checkSeedMarkerFile. A mutant that
// empties checkSeedMarkerFile's body has nothing else to catch this case.
func TestDiagnoseReportsAMalformedMarkerOnALedgeredSeed(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "agent-memory", "ledgered-agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "MEMORY.md")
	// Constructed literally, same discipline as
	// TestDiagnoseReportsAMalformedRulesMarker: an orphan BEGIN with no
	// matching END is exactly what seedMarkersHealthy exists to catch.
	orphan := "some agent notes\n\n" +
		"<!-- ORBEAT-SEED:BEGIN ledgered-agent sha=0123456789ab — managed by orbeat-sync; edit BELOW this block -->\n" +
		"stale body\n"
	if err := os.WriteFile(path, []byte(orphan), 0o644); err != nil {
		t.Fatal(err)
	}
	must(t, saveManifest(home, manifest{Seeds: map[string][]string{"ledgered-agent": {path}}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckMarkers && rep.Findings[i].Path == path {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckMarkers finding for %s, got %+v", path, rep.Findings)
	}
	if found.Severity != SeverityProblem {
		t.Errorf("a malformed marker needs attention — got severity %q", found.Severity)
	}
	if !strings.Contains(found.Detail, "ledgered-agent") {
		t.Errorf("detail must name the subagent, got %q", found.Detail)
	}
	// This case must NOT also surface as a fs-scanned orphan: it IS ledgered,
	// so checkOrphanedSeedsUnder must skip it — asserting this keeps the two
	// paths distinguishable rather than both incidentally green.
	for _, f := range rep.Findings {
		if f.Check == CheckOrphanedBlock && f.Path == path {
			t.Errorf("a ledgered seed's malformed marker must be caught by checkMarkers, not reported as an orphan too: %+v", f)
		}
	}
}

// TestDiagnoseReportsAnUnreadableSeedMarkerFile is checkSeedMarkerFile's
// other branch: a file doctor genuinely cannot read (as opposed to one that
// is merely missing, which is silent by design — see
// TestDiagnoseIsSilentOnHealthyMarkers's sibling in rules.go) must itself be
// a finding, not a silent skip. An operator staring at a "Clean" report while
// doctor quietly failed to read a file would be the worst outcome a
// diagnostic tool can produce.
func TestDiagnoseReportsAnUnreadableSeedMarkerFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced, this test would pass without exercising anything")
	}

	home := t.TempDir()
	dir := filepath.Join(home, "agent-memory", "locked-agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(path, []byte(renderSeedBlock("locked-agent", "some agent memory")), 0o644); err != nil {
		t.Fatal(err)
	}
	must(t, saveManifest(home, manifest{Seeds: map[string][]string{"locked-agent": {path}}}, nil))

	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	// Confirm the permission bit is actually enforced on this filesystem
	// before trusting anything below (mirrors the sync-root unreadable test).
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("mode 000 did not block os.ReadFile here — permissions not enforced, skipping")
	}

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckMarkers && rep.Findings[i].Path == path {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckMarkers finding for the unreadable file %s, got %+v", path, rep.Findings)
	}
	if found.Severity != SeverityProblem {
		t.Errorf("an unreadable managed file needs attention — got severity %q", found.Severity)
	}
	if !strings.Contains(found.Detail, "cannot be read") {
		t.Errorf("detail must say the file could not be read, got %q", found.Detail)
	}
}

// TestDiagnoseWritesNothing is the gate every later task re-runs. It digests
// DIRECTORIES as well as files: a diagnosis that creates ~/.claude while
// inspecting it has changed the thing it was measuring, and a files-only digest
// cannot see that.
func TestDiagnoseWritesNothing(t *testing.T) {
	t.Run("existing tree is untouched", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "agents", "a.md"), []byte("BODY"), 0o644); err != nil {
			t.Fatal(err)
		}
		proj := t.TempDir()
		if err := os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("dev content\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		beforeHome, beforeProj := treeSnapshot(t, home), treeSnapshot(t, proj)
		_ = Diagnose(home, []string{proj}, "", "", "")
		assertTreeUnchanged(t, "sync root", home, beforeHome)
		assertTreeUnchanged(t, "project", proj, beforeProj)
	})

	// An ABSENT sync root is the state where a stray MkdirAll does damage, and
	// it is the one os.MkdirAll on an existing dir cannot demonstrate: that call
	// is a no-op, so a digest sees nothing. First-run is exactly when ~/.claude
	// is missing, so this is the case that matters.
	t.Run("absent sync root stays absent", func(t *testing.T) {
		parent := t.TempDir()
		home := filepath.Join(parent, "never-synced")
		proj := t.TempDir()

		before := treeSnapshot(t, parent)
		_ = Diagnose(home, []string{proj}, "", "", "")

		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Errorf("Diagnose created the sync root it was asked to inspect (stat err: %v)", err)
		}
		assertTreeUnchanged(t, "sync root's parent", parent, before)
	})
}

// findingsForCheck returns every finding a check produced, so a gate can
// assert both presence and absence without hard-coding an index into a list
// other checks also append to.
func findingsForCheck(rep Report, c Check) []Finding {
	var out []Finding
	for _, f := range rep.Findings {
		if f.Check == c {
			out = append(out, f)
		}
	}
	return out
}

// An install.json that will not read stops this machine reporting deployments
// while every other part of the sync keeps working, which is exactly the class
// of silent breakage doctor exists to surface. It is a PROBLEM, not a note:
// the file needs a human, and the obvious repair (delete it) costs this
// machine its identity, so the remedy has to say so.
func TestDiagnoseReportsAnUnreadableInstallFile(t *testing.T) {
	home := t.TempDir()
	ipath := filepath.Join(t.TempDir(), "install.json")
	if err := os.WriteFile(ipath, []byte(`{"installId": "abc`), 0o600); err != nil {
		t.Fatal(err)
	}

	rep := Diagnose(home, nil, ipath, "", "")

	found := findingsForCheck(rep, CheckInstall)
	if len(found) != 1 {
		t.Fatalf("want exactly one CheckInstall finding, got %+v", rep.Findings)
	}
	f := found[0]
	if f.Severity != SeverityProblem {
		t.Fatalf("an unreadable install identity must be a PROBLEM, got %q", f.Severity)
	}
	if f.Path != ipath {
		t.Fatalf("finding path = %q, want %q", f.Path, ipath)
	}
	if !strings.Contains(f.Remedy, "NEW install id") {
		t.Fatalf("the remedy must name what deleting the file costs, got %q", f.Remedy)
	}
	if rep.Problems() == 0 {
		t.Fatal("Problems() = 0 while an unreadable install identity was found")
	}
}

// An unparseable pins.json stops every held pin from applying silently:
// runSync must not guess at a file it cannot read, so doctor names it a
// PROBLEM, the same severity CheckInstall gives its own unreadable case.
func TestDiagnoseReportsAnUnparseablePinsFile(t *testing.T) {
	home := t.TempDir()
	ppath := filepath.Join(t.TempDir(), "pins.json")
	if err := os.WriteFile(ppath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep := Diagnose(home, nil, "", ppath, "")

	found := findingsForCheck(rep, CheckPins)
	if len(found) != 1 {
		t.Fatalf("want exactly one CheckPins finding, got %+v", rep.Findings)
	}
	if found[0].Severity != SeverityProblem {
		t.Fatalf("an unparseable pin file must be a PROBLEM, got %q", found[0].Severity)
	}
	if found[0].Path != ppath {
		t.Fatalf("finding path = %q, want %q", found[0].Path, ppath)
	}
	if found[0].Remedy == "" {
		t.Error("every finding must name what resolves it")
	}
}

// insertRevision numbers revisions from 1, so a pin holding a smaller number
// can only come from a hand-edited file, the same class checkInstall's own
// doc comment assumes for its unreadable case.
func TestDiagnoseReportsAPinBelowRevisionOne(t *testing.T) {
	home := t.TempDir()
	ppath := filepath.Join(t.TempDir(), "pins.json")
	must(t, savePins(ppath, []Pin{{ArtifactID: "a", Type: "skill", Name: "bad", Revision: 0}}))

	rep := Diagnose(home, nil, "", ppath, "")

	found := findingsForCheck(rep, CheckPins)
	if len(found) != 1 {
		t.Fatalf("want exactly one CheckPins finding, got %+v", rep.Findings)
	}
	if found[0].Severity != SeverityProblem {
		t.Fatalf("a pin below revision 1 must be a PROBLEM, got %q", found[0].Severity)
	}
	if !strings.Contains(found[0].Detail, "skill/bad") {
		t.Errorf("detail must name the offending pin, got %q", found[0].Detail)
	}
}

// The healthy and the never-pinned cases are both SILENT. A machine that has
// never run 'orbeat-sync pin' has no pins.json, and turning that into a
// finding would put a permanent line in front of every developer who has
// never used the feature.
func TestDiagnoseIsSilentOnAHealthyOrAbsentPinsFile(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()

	absent := filepath.Join(dir, "pins.json")
	if found := findingsForCheck(Diagnose(home, nil, "", absent, ""), CheckPins); len(found) != 0 {
		t.Fatalf("an absent pins.json produced %+v, want nothing", found)
	}

	must(t, savePins(absent, []Pin{{ArtifactID: "a", Type: "skill", Name: "good", Revision: 3}}))
	rep := Diagnose(home, nil, "", absent, "")
	if found := findingsForCheck(rep, CheckPins); len(found) != 0 {
		t.Fatalf("a valid pins.json produced %+v, want nothing", found)
	}
	if rep.Problems() != 0 {
		t.Fatalf("a healthy tree with a valid pins file must report zero problems, got %+v", rep.Findings)
	}
}

// The healthy and the never-reported cases are both SILENT. A machine that has
// never filed a report has no install.json, and turning that into a finding
// would put a permanent line in front of every user of a server that does not
// record deployments at all.
func TestDiagnoseIsSilentOnAHealthyOrAbsentInstallFile(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()

	absent := filepath.Join(dir, "install.json")
	if found := findingsForCheck(Diagnose(home, nil, absent, "", ""), CheckInstall); len(found) != 0 {
		t.Fatalf("an absent install.json produced %+v, want nothing", found)
	}

	if _, err := EnsureInstallID(absent); err != nil {
		t.Fatal(err)
	}
	rep := Diagnose(home, nil, absent, "", "")
	if found := findingsForCheck(rep, CheckInstall); len(found) != 0 {
		t.Fatalf("a valid install.json produced %+v, want nothing", found)
	}
	if rep.Problems() != 0 {
		t.Fatalf("a healthy tree with a valid install id must report zero problems, got %+v", rep.Findings)
	}
}

// An unledgered SEED block is a note, not a problem, because the seed strip
// pass is not ledger-driven: seed.go's candidate set is the UNION of the
// ledger and a filesystem scan of <claudeDir>/agent-memory and each
// registered project's .claude/agent-memory, which are exactly the roots
// checkOrphanedSeedsUnder scans. Every block this check can report is
// therefore already a strip candidate on the next ordinary sync.
//
// The test does not stop at the severity: it runs the sync the note promises
// and asserts the block is gone, so the claim in the finding is checked
// rather than its wording. Telling an operator to hand-edit a file that
// fixes itself is what doctor's own Severity doc calls the main way a
// diagnostic does harm.
func TestDiagnoseCallsAnOrphanedSeedBlockANoteBecauseTheNextSyncStripsIt(t *testing.T) {
	home := t.TempDir()
	path := writeOrphanSeedCandidate(t, filepath.Join(home, "agent-memory"), "orphan-agent")

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckOrphanedBlock && rep.Findings[i].Path == path {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckOrphanedBlock finding for %s, got %+v", path, rep.Findings)
	}
	if found.Severity != SeverityNote {
		t.Errorf("the seed strip pass fs-scans this same root, so this self-corrects: want severity %q, got %q", SeverityNote, found.Severity)
	}
	if strings.Contains(found.Remedy, "by hand") {
		t.Errorf("the remedy must not send an operator to hand-edit a block the next sync removes: %q", found.Remedy)
	}
	if !strings.Contains(found.Remedy, "orbeat-sync sync") {
		t.Errorf("the remedy must name the sync that removes it: %q", found.Remedy)
	}
	if got := rep.Problems(); got != 0 {
		t.Errorf("a self-correcting condition must not count as a problem: Problems() = %d, want 0", got)
	}

	// The reproduction, and the reason the severity above is what it is: one
	// ordinary ReconcileSeeds with nothing entitled removes the very block
	// the old finding told the operator to delete by hand.
	res, err := ReconcileSeeds(home, nil, nil, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Stripped != 1 {
		t.Fatalf("the next sync must strip the block the note promises it strips, got Stripped=%d", res.Stripped)
	}
	if strings.Contains(readFileT(t, path), "ORBEAT-SEED") {
		t.Fatal("the block survived the sync, so the note's claim would be false")
	}
}

// writeStrandedSeed writes a healthy ORBEAT-SEED block for name into
// <proj>/.claude/agent-memory/<name>/MEMORY.md and returns the path. The
// caller decides whether proj is registered; that decision is the whole
// subject of the tests below.
func writeStrandedSeed(t *testing.T, proj, name string) string {
	t.Helper()
	dir := filepath.Join(proj, ".claude", "agent-memory", name)
	must(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "MEMORY.md")
	must(t, os.WriteFile(path, []byte(renderSeedBlock(name, "governed body")+"\nagent notes\n"), 0o644))
	return path
}

// TestDiagnoseReportsAStrandedSeedLedgerEntry pins the gap the seed
// containment narrowing opened: ReconcileSeeds picks each candidate's
// containment boundary out of the roots the RUN was handed (claudeDir plus
// every registered project), so a ledger path under none of them is skipped
// and its entry preserved. The block is then stranded twice over, because
// doctor's own orphan scan only walks those same roots and never sees it
// either.
//
// The test does not stop at the finding. It runs the sync and asserts the
// block SURVIVES, so what is checked is the claim ("nothing removes this on
// its own") rather than its wording.
func TestDiagnoseReportsAStrandedSeedLedgerEntry(t *testing.T) {
	home := t.TempDir()
	unregistered := t.TempDir()
	path := writeStrandedSeed(t, unregistered, "stray")
	must(t, saveManifest(home, manifest{Seeds: map[string][]string{"stray": {path}}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckStrandedEntry && rep.Findings[i].Path == path {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("want a CheckStrandedEntry finding for %s, got %+v", path, rep.Findings)
	}
	if found.Severity != SeverityProblem {
		t.Errorf("nothing removes a stranded block on its own, so it needs action: want severity %q, got %q", SeverityProblem, found.Severity)
	}
	if !strings.Contains(found.Remedy, "orbeat-sync project add "+unregistered) {
		t.Errorf("the remedy must name the project to register: %q", found.Remedy)
	}
	if !strings.Contains(found.Remedy, "orbeat-sync project remove "+unregistered) {
		t.Errorf("the remedy must name the removal that strips the block: %q", found.Remedy)
	}

	// The reproduction, and the reason the severity above is what it is: an
	// ordinary sync with nothing entitled leaves the block exactly where it is.
	res, err := ReconcileSeeds(home, nil, nil, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Stripped != 0 {
		t.Fatalf("the strip pass must skip an untrusted ledger path, got Stripped=%d", res.Stripped)
	}
	if !strings.Contains(readFileT(t, path), "ORBEAT-SEED") {
		t.Fatal("the sync removed the block, so this finding would be a false positive")
	}
}

// TestDiagnoseDoesNotCallASeedUnderARegisteredProjectStranded is the negative
// half, and its first arm exists so the second cannot pass vacuously. A check
// that derives its subjects from a manifest reports nothing when the
// derivation is empty, which looks identical to reporting nothing because the
// tree is healthy. The unregistered arm proves this exact fixture IS a subject
// the check examines before the registered arm asserts silence on it.
func TestDiagnoseDoesNotCallASeedUnderARegisteredProjectStranded(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	path := writeStrandedSeed(t, proj, "stray")
	must(t, saveManifest(home, manifest{Seeds: map[string][]string{"stray": {path}}}, nil))

	// Non-vacuity: unregistered, this fixture must produce the finding.
	if got := findingsForCheck(Diagnose(home, nil, "", "", ""), CheckStrandedEntry); len(got) != 1 {
		t.Fatalf("the fixture must be a subject this check examines, or the assertion below proves nothing: want 1 stranded finding with no project registered, got %+v", got)
	}

	// Registered, the strip pass reaches it, so nothing is stranded.
	if got := findingsForCheck(Diagnose(home, []string{proj}, "", "", ""), CheckStrandedEntry); len(got) != 0 {
		t.Errorf("a seed under a registered project is reached by the strip pass, so it is not stranded: got %+v", got)
	}
}

// TestRulesLedgerEntriesDoNotStrand records what the rules half actually does
// now (B23 fix): rules used to be immune to stranding, because ReconcileRules
// opened its containment root directly AT the ledgered project root with no
// check that the root was one this run actually registered — the exact
// escape trustedSeedBoundary closed for seeds. That made this test's OLD
// assertion (Stripped==1 for an unregistered project) the documented shape
// of the vulnerability, not a safe design choice: a tampered or stale
// manifest entry could point the rules strip pass at any directory and have
// its ORBEAT-RULES block silently removed. rules.go now consults a trusted
// set (registered projects) the same way ReconcileSeeds consults
// trustedSeedBoundary, so an unregistered project's block is preserved
// instead — rules CAN strand now, exactly like seeds can.
//
// doctor's checkStrandedEntries still does not walk the Rules ledger (see its
// own doc comment for why that is a known, separate gap rather than a
// contradiction): the assertion below that Diagnose reports nothing for this
// fixture is therefore still true, but for a different reason than before —
// not because rules cannot strand, but because doctor does not yet check for
// it.
func TestRulesLedgerEntriesDoNotStrand(t *testing.T) {
	home := t.TempDir()
	unregistered := t.TempDir()
	agents := filepath.Join(unregistered, "AGENTS.md")
	must(t, os.WriteFile(agents, []byte("dev content\n\n"+renderRulesBlock("## org\n\nrule body")), 0o644))
	must(t, saveManifest(home, manifest{Rules: []string{unregistered}}, nil))

	res, err := ReconcileRules(home, nil, nil, nil)
	if err != nil {
		t.Fatalf("reconcile rules: %v", err)
	}
	if res.Stripped != 0 {
		t.Fatalf("an unregistered project's rules ledger entry must be preserved, not stripped, got Stripped=%d (warnings %v, failures %v)", res.Stripped, res.Warnings, res.Failures)
	}
	if !strings.Contains(readFileT(t, agents), "ORBEAT-RULES") {
		t.Fatal("the block must survive: an unregistered project is untrusted, and the strip pass must not touch it")
	}

	// doctor does not (yet) walk the Rules ledger for stranded entries — see
	// checkStrandedEntries' own doc comment — so it stays silent about this
	// fixture even though the block above is, in fact, now stranded.
	must(t, saveManifest(home, manifest{Rules: []string{unregistered}}, nil))
	if got := findingsForCheck(Diagnose(home, nil, "", "", ""), CheckStrandedEntry); len(got) != 0 {
		t.Errorf("doctor does not walk the Rules ledger yet, so it must report nothing here — got %+v (if this now fires, checkStrandedEntries has been extended to Rules and this comment/test should be updated together)", got)
	}
}

// TestDiagnoseDoesNotStrandALedgerLineWhoseFileIsGone pins the presence
// requirement. A ledger entry outside every trusted root whose MEMORY.md is
// no longer on disk holds no governed block, so reporting it would send an
// operator to register and de-register a project in order to strip nothing.
// The first arm is the non-vacuity guard: with the block present this exact
// fixture IS reported, so the silence below is about the missing file and not
// about the fixture falling outside the check.
func TestDiagnoseDoesNotStrandALedgerLineWhoseFileIsGone(t *testing.T) {
	home := t.TempDir()
	unregistered := t.TempDir()
	path := writeStrandedSeed(t, unregistered, "stray")
	must(t, saveManifest(home, manifest{Seeds: map[string][]string{"stray": {path}}}, nil))

	if got := findingsForCheck(Diagnose(home, nil, "", "", ""), CheckStrandedEntry); len(got) != 1 {
		t.Fatalf("the fixture must be a subject this check examines, or the assertion below proves nothing: want 1, got %+v", got)
	}

	must(t, os.Remove(path))
	if got := findingsForCheck(Diagnose(home, nil, "", "", ""), CheckStrandedEntry); len(got) != 0 {
		t.Errorf("no block is on disk, so nothing is stranded and the remedy would strip nothing: got %+v", got)
	}
}

// TestStrandedSeedRemedyNamesAProjectOnlyWhenThereIsOne: validSeedPath accepts
// an agent-memory tree anywhere, not only under a project's .claude, so the
// generic branch is live rather than defensive. Four directories up from
// /Users/bob/agent-memory/x/MEMORY.md is /Users, and a remedy that told an
// operator to run 'project add /Users' would put her whole home directory
// under sync's management to clean up one file.
func TestStrandedSeedRemedyNamesAProjectOnlyWhenThereIsOne(t *testing.T) {
	projectScope := strandedSeedRemedy("/srv/repo/.claude/agent-memory/x/MEMORY.md")
	if !strings.Contains(projectScope, "orbeat-sync project add /srv/repo") {
		t.Errorf("a project-scope path has a project to name: %q", projectScope)
	}

	loose := strandedSeedRemedy("/Users/bob/agent-memory/x/MEMORY.md")
	if strings.Contains(loose, "/Users") {
		t.Errorf("four directories up from a non-project layout is an ancestor, not a project, and registering it is worse than saying nothing: %q", loose)
	}
	if !strings.Contains(loose, "orbeat-sync project add") {
		t.Errorf("the generic branch must still name the command that clears it: %q", loose)
	}
}

// TestLedgerDriftDoesNotPromiseSelfCorrectionForAnUntrustedPath pins the one
// case where "no action needed, this self-corrects" is a lie.
//
// checkLedgerDrift notes a ledgered seed file that is not on disk and tells the
// operator the entry "drops on its own". That is true while the path lies under
// a trusted root: the strip pass reaches it, nothing marks it failed, and the
// preservation loop does not re-add it. For a path under NO trusted root it is
// false in the opposite direction: ReconcileSeeds skips the entry before it
// ever touches the filesystem, marks it failedPaths, and preserves it forever,
// so the entry outlives every sync the operator will ever run.
//
// The behaviour is correct and is NOT what this test changes. The skip happens
// before any file I/O precisely so an untrusted path is never touched, which
// means the run genuinely cannot know the file is gone. What was wrong is
// doctor promising a self-correction that cannot happen.
func TestLedgerDriftDoesNotPromiseSelfCorrectionForAnUntrustedPath(t *testing.T) {
	home := t.TempDir()
	unregistered := t.TempDir()
	// A ledger entry whose shape is valid and whose file does NOT exist, under
	// no trusted root: not stranded (nothing is on disk to strip) and not
	// self-correcting (the entry is preserved).
	gone := filepath.Join(unregistered, ".claude", "agent-memory", "vanished", "MEMORY.md")
	must(t, saveManifest(home, manifest{Seeds: map[string][]string{"vanished": {gone}}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	var drift *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckLedgerDrift && rep.Findings[i].Path == gone {
			drift = &rep.Findings[i]
		}
	}
	if drift == nil {
		t.Fatalf("the fixture must produce a ledger-drift finding, or the assertions below prove nothing: got %+v", rep.Findings)
	}
	if strings.Contains(drift.Remedy, "no action needed") || strings.Contains(drift.Detail, "drops on its own") {
		t.Errorf("this entry is preserved by every future sync, so promising self-correction is false:\n  detail: %q\n  remedy: %q", drift.Detail, drift.Remedy)
	}

	// The reproduction behind the assertion: a sync leaves the entry in place.
	if _, err := ReconcileSeeds(home, nil, nil, nil); err != nil {
		t.Fatalf("ReconcileSeeds: %v", err)
	}
	m, err := loadManifest(home)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if len(m.Seeds["vanished"]) == 0 {
		t.Fatal("the entry dropped after all, so the remedy above was right and this test is wrong")
	}
}

// A hand-edited projects.json can carry a shape-invalid entry (validProjectPath:
// not absolute, not already-clean, or empty) — B25 already made LoadProjects
// drop it rather than turn it into a load error, so every OTHER, valid entry
// keeps working. What B25 left open is visibility: the drop is silent, so the
// project it names silently stops syncing with nothing anywhere telling the
// developer why. This is a PROBLEM, not a note: unlike a malformed Rules/Seeds
// ledger entry (which self-heals — the owning reconciler warns and drops it
// from ITS OWN ledger on its next save), nothing here ever repairs a
// projects.json entry — the file is user-authored and orbeat-sync never
// rewrites an existing line to fix its shape, only silently skips it forever.
func TestDiagnoseReportsAMalformedProjectsFileEntry(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	good := filepath.Join(dir, "good-proj")
	must(t, os.MkdirAll(good, 0o755))

	pj := filepath.Join(dir, "cfg", "projects.json")
	must(t, os.MkdirAll(filepath.Dir(pj), 0o700))
	raw := `{"projects":["` + good + `", "relative/path", "."]}`
	must(t, os.WriteFile(pj, []byte(raw), 0o644))

	rep := Diagnose(home, nil, "", "", pj)

	found := findingsForCheck(rep, CheckProjectsFile)
	if len(found) != 2 {
		t.Fatalf("want exactly 2 CheckProjectsFile findings (one per malformed entry), got %d: %+v", len(found), rep.Findings)
	}
	gotPaths := map[string]bool{}
	for _, f := range found {
		if f.Severity != SeverityProblem {
			t.Errorf("a malformed projects.json entry must be a PROBLEM (nothing self-heals it), got %q for %q", f.Severity, f.Path)
		}
		if f.Remedy == "" {
			t.Error("every finding must name what resolves it")
		}
		gotPaths[f.Path] = true
	}
	if !gotPaths["relative/path"] || !gotPaths["."] {
		t.Fatalf("findings must name the actual malformed entries, got %+v", found)
	}
	if gotPaths[good] {
		t.Fatalf("the shape-valid entry must not be reported, got %+v", found)
	}
	if rep.Problems() == 0 {
		t.Fatal("Problems() = 0 while a malformed projects.json entry was found")
	}
}

// The healthy and the absent cases are both SILENT, mirroring CheckPins and
// CheckInstall: a machine that has never registered a project has no
// projects.json, and a well-formed one is not news.
func TestDiagnoseIsSilentOnAHealthyOrAbsentProjectsFile(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()

	absent := filepath.Join(dir, "projects.json")
	if found := findingsForCheck(Diagnose(home, nil, "", "", absent), CheckProjectsFile); len(found) != 0 {
		t.Fatalf("an absent projects.json produced %+v, want nothing", found)
	}

	good := filepath.Join(dir, "good-proj")
	must(t, os.MkdirAll(good, 0o755))
	if _, err := AddProject(absent, good, nil); err != nil {
		t.Fatal(err)
	}
	rep := Diagnose(home, nil, "", "", absent)
	if found := findingsForCheck(rep, CheckProjectsFile); len(found) != 0 {
		t.Fatalf("a valid projects.json produced %+v, want nothing", found)
	}
	if rep.Problems() != 0 {
		t.Fatalf("a healthy tree with a valid projects.json must report zero problems, got %+v", rep.Findings)
	}
}
