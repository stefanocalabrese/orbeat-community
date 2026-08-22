package syncclient

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Check names the diagnosis a Finding came from. It is a closed set: the
// renderer groups by it and the JSON consumer switches on it.
type Check string

const (
	CheckSyncRoot Check = "sync-root"
	// CheckProject flags a registered project root doctor could not stat —
	// sync silently skips it on every run.
	CheckProject Check = "project"
	// CheckPreservedEntry flags a ledger (Rules/Seeds) entry whose project
	// root is currently unreachable. This is NOT the same finding as
	// CheckProject: since v1.15.0 the reconcilers deliberately KEEP such an
	// entry — an unmounted volume looks identical to a genuine delete, so
	// dropping the entry now would permanently orphan a managed block that
	// is still on disk. Always SeverityNote (see the Severity doc comment).
	// Suppressed for a path that already carries a CheckProject PROBLEM (a
	// registered project doctor also cannot reach) — the two would otherwise
	// contradict each other for the identical path: "reconnect or remove it"
	// next to "no action needed".
	CheckPreservedEntry Check = "preserved-entry"
	// CheckLedgerDrift flags a manifest Files entry with no backing file on
	// disk. Sync recreates it on the next run — that part is not damage — but
	// nothing else surfaces that something removed it out of band.
	CheckLedgerDrift Check = "ledger-drift"
	// CheckOrphanedBlock flags a managed ORBEAT-SEED or ORBEAT-RULES block
	// found on disk whose containing path (Seeds: name+path; Rules: project
	// root) is absent from the corresponding manifest ledger. The strip pass
	// works FROM the ledger, so an unlisted block is invisible to every
	// future sync: nothing will ever remove it. doctor can only discover
	// this in REGISTERED projects — a de-registered project's orphans need a
	// full filesystem scan doctor does not perform, so an empty result here
	// is not proof there are none; each finding says so in its own Remedy.
	CheckOrphanedBlock Check = "orphaned-block"
	// CheckMarkers flags an ORBEAT-managed file whose sentinel markers are
	// malformed (orphan or duplicate BEGIN/END), or that doctor could not even
	// read. A malformed marker makes sync refuse to touch that file on every
	// run, silently — the refusal is a warning in the middle of a run nobody
	// reads twice.
	CheckMarkers Check = "markers"
	// CheckManifest flags a sync manifest — or a specific ledger entry inside
	// an otherwise-parseable one — that sync cannot trust. Two flavors:
	//   - the manifest file itself will not parse (SeverityProblem). Distinct
	//     from a MISSING manifest (not a finding — see checkManifest's doc
	//     comment): this makes sync abort outright with exit 2, so it is
	//     arguably the single most valuable thing doctor can report.
	//   - an individual Files/Rules/Seeds entry that fails the same
	//     validation the reconcilers themselves apply (resolveContained's
	//     traversal guard, validRulesPath, validSeedPath). A traversing Files
	//     entry is itself fatal — Reconcile returns the identical error and
	//     every sync aborts at exit 2 — so it is a SeverityProblem too. A
	//     shape-invalid Rules/Seeds entry is not fatal: the owning reconciler
	//     warns (or says nothing) and drops it from the ledger on its own next
	//     run, so it is a SeverityNote.
	CheckManifest Check = "manifest"
	// CheckAuth is the one line doctor devotes to authentication. Unlike every
	// other check, it does not inspect the filesystem and is not conditioned on
	// any state: Diagnose emits it unconditionally, on every call, healthy tree
	// or not. It exists so a user whose real problem is an expired token is
	// pointed at 'orbeat-sync status' — which already distinguishes not-logged-in,
	// valid, expired-with-refresh and expired-without-refresh — instead of
	// duplicating that state machine here, where it could drift from the one in
	// loadValidToken. Always SeverityNote: it must never turn a clean tree into
	// one with problems (see the Severity doc comment and Report.Problems).
	CheckAuth Check = "auth"
)

// Severity separates "this needs your attention" from "this is working as
// designed and you should know it is happening". Getting that distinction
// wrong is the main way a diagnostic does harm: a preserved ledger entry is
// CORRECT behaviour, and reporting it as a problem sends users to delete state
// that is protecting them.
type Severity string

const (
	SeverityProblem Severity = "problem"
	SeverityNote    Severity = "note"
)

// Finding is one thing doctor observed. Remedy is not optional: a diagnosis an
// operator cannot act on is a diagnosis that wastes their time.
type Finding struct {
	Check    Check    `json:"check"`
	Severity Severity `json:"severity"`
	Path     string   `json:"path"`
	Detail   string   `json:"detail"`
	Remedy   string   `json:"remedy"`
}

// Report is the whole diagnosis. It is a value: Diagnose performs reads, builds
// this, and returns. Nothing downstream needs the filesystem.
type Report struct {
	Findings []Finding `json:"findings"`
}

// Problems counts findings that need action, excluding notes.
func (r Report) Problems() int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == SeverityProblem {
			n++
		}
	}
	return n
}

// Diagnose inspects local sync state and reports what it finds. It NEVER
// writes: no MkdirAll, no file creation, no temp files. Every check reads.
//
// It is also offline by construction — nothing here takes a context, a client
// or a token. doctor has to work on the machine where things are broken, and an
// expired token is exactly when a diagnosis is wanted. Because of that, the
// Report this returns is NEVER empty: checkAuth unconditionally appends one
// CheckAuth note deferring auth diagnosis to 'orbeat-sync status', on every
// call, healthy tree or not. A caller checking whether anything needs
// attention must use Report.Problems(), not len(Findings).
func Diagnose(claudeDir string, projects []string) Report {
	var r Report
	projects = dedupeProjects(projects)
	r.checkSyncRoot(claudeDir)
	r.checkProjects(projects)

	// The manifest is read exactly ONCE here and threaded into every check
	// below, instead of each check calling loadManifest for itself.
	// saveManifest writes temp+rename, so a `sync` running concurrently
	// replaces the file atomically — four independent reads could each
	// observe a different version, judging ledger drift against v1 while
	// orphaned blocks are judged against v2: a transient false positive on a
	// perfectly healthy machine, and the hardest kind to reproduce from a bug
	// report. One snapshot makes every check's verdict internally consistent,
	// and drops three of the four os.OpenRoot round-trips loadManifest
	// performed per Diagnose call. The error is threaded too, unevaluated —
	// checkManifest is still the ONLY check that turns it into a finding;
	// every other check just bails out on a non-nil err, exactly as it did
	// when it called loadManifest itself.
	m, err := loadManifest(claudeDir)

	r.checkPreservedEntries(claudeDir, m, err)
	r.checkLedgerDrift(claudeDir, m, err)
	r.checkOrphanedBlocks(claudeDir, projects, m, err)
	r.checkMarkers(projects, m, err)
	r.checkManifest(claudeDir, err)
	r.checkAuth()
	return r
}

// dedupeProjects collapses duplicate registered-project paths — comparing
// filepath.Clean'd values, keeping each entry's first (original) spelling —
// before any check runs. AddProject (projects.go) already refuses to
// register the same path twice, so only a hand-edited projects.json reaches
// this; without it, every project-scoped check (unreachable project,
// preserved entry, ledger drift, orphaned block, marker) reports the
// identical path twice, reading as two distinct problems instead of one.
func dedupeProjects(projects []string) []string {
	if len(projects) == 0 {
		return projects
	}
	seen := make(map[string]bool, len(projects))
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		clean := filepath.Clean(p)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, p)
	}
	return out
}

func (r *Report) add(f Finding) { r.Findings = append(r.Findings, f) }

func (r *Report) checkSyncRoot(claudeDir string) {
	st, err := os.Stat(claudeDir)
	switch {
	case os.IsNotExist(err):
		r.add(Finding{
			Check: CheckSyncRoot, Severity: SeverityProblem, Path: claudeDir,
			Detail: "the sync root does not exist — nothing has been synced to this machine yet",
			Remedy: "run 'orbeat-sync sync' to create it and fetch your entitled artifacts",
		})
	case err != nil:
		r.add(Finding{
			Check: CheckSyncRoot, Severity: SeverityProblem, Path: claudeDir,
			Detail: fmt.Sprintf("the sync root cannot be read: %v", err),
			Remedy: "check the path's permissions and ownership",
		})
	case !st.IsDir():
		r.add(Finding{
			Check: CheckSyncRoot, Severity: SeverityProblem, Path: claudeDir,
			Detail: "the sync root exists but is not a directory",
			Remedy: "move or remove the file at that path, then run 'orbeat-sync sync'",
		})
	default:
		// os.Stat succeeding is NOT proof the root is usable: statting a
		// directory only needs search permission on its PARENT, not on the
		// directory itself, so a mode-000 claudeDir sails through the switch
		// above untouched. os.ReadDir needs read+execute on claudeDir itself —
		// the same bits loadManifest's os.OpenRoot needs — so it fails for
		// exactly the condition that otherwise surfaces, one check later, as a
		// misattributed manifest problem (see checkManifest).
		if _, err := os.ReadDir(claudeDir); err != nil {
			r.add(Finding{
				Check: CheckSyncRoot, Severity: SeverityProblem, Path: claudeDir,
				Detail: fmt.Sprintf("the sync root cannot be read: %v", err),
				Remedy: "check the path's permissions and ownership",
			})
		}
	}
}

// checkProjects flags every registered project doctor cannot stat as a
// directory. Unlike a preserved ledger entry (checkPreservedEntries), this IS
// a problem: the reconcilers silently skip an unreachable registered project
// on every run, and the operator may not know sync has stopped covering it.
func (r *Report) checkProjects(projects []string) {
	for _, p := range projects {
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			r.add(Finding{
				Check: CheckProject, Severity: SeverityProblem, Path: p,
				Detail: "this registered project is unreachable — orbeat-sync silently skips it on every run",
				Remedy: "reconnect the volume or fix the path if it's temporary; if it's gone for good, run 'orbeat-sync project remove' to drop it",
			})
		}
	}
}

// checkPreservedEntries cross-references the sync manifest's Rules and Seeds
// ledgers against the filesystem and reports, as NOTES (never problems), any
// entry whose project root is currently unreachable. Since v1.15.0 the
// reconcilers deliberately preserve such entries — an unmounted volume is
// indistinguishable from a genuine delete, so dropping the entry now would
// orphan a managed block that may still be on disk. Reporting these as
// problems would send an operator to delete the exact state that protects
// them; see the Severity doc comment.
//
// A missing manifest is NOT a finding here: a machine that has never synced
// has none, and checkSyncRoot already covers an absent sync root. A manifest
// that fails to load (corrupt/unreadable) is likewise not reported by THIS
// check — that's checkManifest's job, reported separately (CheckManifest).
func (r *Report) checkPreservedEntries(claudeDir string, m manifest, err error) {
	if err != nil {
		return
	}

	// Diagnose always runs checkProjects before this, so its findings — if
	// any — are already in r.Findings. A registered-but-unreachable project
	// gets a CheckProject PROBLEM ("reconnect ... or run 'project remove'");
	// reporting the SAME path here too, as a "no action needed" note,
	// contradicts that in the same report and leaves the operator with
	// nothing actionable — see the CheckPreservedEntry doc comment.
	unreachableProjects := map[string]bool{}
	for _, f := range r.Findings {
		if f.Check == CheckProject {
			unreachableProjects[filepath.Clean(f.Path)] = true
		}
	}

	reported := map[string]bool{}
	notePreserved := func(root string) {
		if reported[root] || unreachableProjects[root] {
			return
		}
		reported[root] = true
		r.add(Finding{
			Check: CheckPreservedEntry, Severity: SeverityNote, Path: root,
			Detail: "the sync ledger is holding an entry for this unreachable project on purpose, so its managed block can be stripped once the path is reachable again",
			Remedy: "no action needed — reconnect the path and run 'orbeat-sync sync' to self-heal it, or 'orbeat-sync project remove' if it's gone for good",
		})
	}

	// A Rules entry IS a project root.
	for _, p := range m.Rules {
		if !validRulesPath(p) {
			// Not fatal: ReconcileRules' own strip pass hits this exact
			// check, warns "ignoring malformed rules ledger entry", and drops
			// the entry from the ledger on this same run — self-healing, so a
			// note (not a problem) is the honest severity. Discarding this
			// silently, though, meant nothing ever told the operator that
			// entry was there or that it was about to vanish.
			r.add(Finding{
				Check: CheckManifest, Severity: SeverityNote, Path: p,
				Detail: fmt.Sprintf("the sync ledger's rules entry %q is not a valid project-root path — the next 'orbeat-sync sync' will warn and drop it from the ledger on its own", p),
				Remedy: "no action needed — a malformed entry self-heals on the next sync; if rules stopped syncing for a project you expect, run 'orbeat-sync sync' and check its warnings",
			})
			continue
		}
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			notePreserved(filepath.Clean(p))
		}
	}

	// A Seeds entry is a MEMORY.md path, not a project root — derive the
	// project via seedBoundary (the same derivation the reconciler's own
	// strip pass uses). A user-scope seed (boundary == claudeDir) is not
	// project-bound, so it is out of scope for this check.
	cleanClaudeDir := filepath.Clean(claudeDir)
	for name, paths := range m.Seeds {
		for _, p := range paths {
			if !validSeedPath(p) {
				// Not fatal: ReconcileSeeds' strip pass shape-checks the same
				// way and simply excludes the entry from its candidates, so
				// it drops out of the ledger — silently, not even a warning —
				// on the next sync. A note tells the operator what THIS check
				// could not verify (reachability) rather than the discard
				// staying invisible altogether.
				r.add(Finding{
					Check: CheckManifest, Severity: SeverityNote, Path: p,
					Detail: fmt.Sprintf("the sync ledger's seed entry %q for %q is not a valid per-subagent memory path — doctor cannot tell whether its project is reachable, and the next 'orbeat-sync sync' will silently drop it from the ledger", p, name),
					Remedy: "no action needed — a malformed entry self-heals on the next sync; if this subagent's memory stopped syncing for a project you expect, run 'orbeat-sync sync'",
				})
				continue
			}
			boundary := seedBoundary(claudeDir, p)
			if boundary == cleanClaudeDir {
				continue
			}
			if st, err := os.Stat(boundary); err != nil || !st.IsDir() {
				notePreserved(boundary)
			}
		}
	}
}

// checkLedgerDrift cross-references the manifest's Files ledger — the
// skill/subagent files Reconcile renders — and its Seeds ledger — the
// per-subagent MEMORY.md files ReconcileSeeds writes — against the
// filesystem, reporting any tracked path with no backing file on disk.
//
// The two ledgers get different severities for the identical shape of drift.
// A Files entry is wholly orbeat-rendered content: the next sync recreates
// it (not damage), but the operator cannot see that coming without being
// told, and it may indicate something deleted the file out of band —
// SeverityProblem. A Seeds entry's MEMORY.md is NOT wholly orbeat's —
// everything outside the managed block belongs to the subagent/developer
// (see rules.go/seed.go's own opening comments) — sitting inside a project
// the developer owns, so deleting the whole file is an ordinary, legitimate
// action, not out-of-band interference with orbeat-managed state. Reporting
// that as a problem would be the same false-positive class defect A's fix
// exists to avoid. It is not silent, though, and it self-corrects either
// way: ReconcileSeeds' write pass recreates it next sync if the subagent is
// still entitled (a missing file reads as an empty existing block), and its
// strip pass treats a missing file as nothing to strip (no warning, no
// error) — so a de-entitled seed's ledger entry simply drops next sync
// instead of orphaning anything.
//
// A ledgered seed under a project root that is itself unreachable is
// deliberately skipped here — checkPreservedEntries already reports that as
// its own note with a "reconnect or remove" remedy, and reporting the same
// unmounted volume a second time as "this file is missing" would be
// redundant noise about one fault rather than two distinct findings.
//
// A missing manifest is not a finding here (same reasoning as
// checkPreservedEntries), and a manifest that fails to load is checkManifest's
// job, not this one.
func (r *Report) checkLedgerDrift(claudeDir string, m manifest, err error) {
	if err != nil {
		return
	}
	for _, rel := range m.Files {
		full, err := resolveContained(claudeDir, rel)
		if err != nil {
			// NOT harmless: resolveContained's error here is the exact
			// fatalError Reconcile returns for this same entry — every
			// 'orbeat-sync sync' aborts at exit 2 before seeds or rules ever
			// run. Discarding it left doctor reporting Clean on a hard-down
			// product; report it under CheckManifest, the check that already
			// owns "a manifest sync cannot trust".
			r.add(Finding{
				Check: CheckManifest, Severity: SeverityProblem, Path: rel,
				Detail: fmt.Sprintf("the sync ledger's file entry %q escapes the sync root — every 'orbeat-sync sync' run aborts at exit 2 until this entry is repaired", rel),
				Remedy: "if you have a backup, restore it; otherwise edit the manifest to remove this entry, or delete the manifest entirely and run 'orbeat-sync sync' — it will rebuild it and re-fetch your entitled artifacts",
			})
			continue
		}
		if _, err := os.Stat(full); err != nil {
			r.add(Finding{
				Check: CheckLedgerDrift, Severity: SeverityProblem, Path: full,
				Detail: "the sync ledger tracks this file but it is not on disk — something removed it outside of orbeat-sync",
				Remedy: "run 'orbeat-sync sync' to recreate it; if the removal was intentional, no action is needed",
			})
		}
	}

	cleanClaudeDir := filepath.Clean(claudeDir)
	for name, paths := range m.Seeds {
		for _, p := range paths {
			if !validSeedPath(p) {
				continue // checkPreservedEntries/checkMarkers already report a shape-invalid entry
			}
			boundary := seedBoundary(claudeDir, p)
			if boundary != cleanClaudeDir {
				if st, err := os.Stat(boundary); err != nil || !st.IsDir() {
					continue // the project itself is unreachable — checkPreservedEntries owns that finding
				}
			}
			if _, err := os.Stat(p); err != nil {
				r.add(Finding{
					Check: CheckLedgerDrift, Severity: SeverityNote, Path: p,
					Detail: fmt.Sprintf("the sync ledger tracks a seed for %q here but it is not on disk — if the subagent is still entitled, 'orbeat-sync sync' recreates it; otherwise the ledger entry drops on its own", name),
					Remedy: "no action needed — this self-corrects on the next 'orbeat-sync sync'",
				})
			}
		}
	}
}

// checkOrphanedBlocks fs-scans for managed ORBEAT-SEED/ORBEAT-RULES blocks the
// manifest ledger does not list — content the strip pass, which works FROM
// the ledger, will never remove. Seeds reuse scanSeedFiles (seed.go), the
// strip pass's own fs-scan, over <claudeDir>/agent-memory and each registered
// project's .claude/agent-memory; rules need no scanner — they live at
// exactly two known paths per project, AGENTS.md and CLAUDE.md.
//
// A missing manifest is not a finding here (loadManifest's error is
// checkManifest's job); an unloadable one means there is no trustworthy
// ledger to compare against, so this check is silent rather than guessing.
func (r *Report) checkOrphanedBlocks(claudeDir string, projects []string, m manifest, err error) {
	if err != nil {
		return // surfaced separately by checkManifest
	}

	r.checkOrphanedSeedsUnder(filepath.Join(claudeDir, "agent-memory"), m)
	for _, p := range projects {
		r.checkOrphanedSeedsUnder(filepath.Join(p, ".claude", "agent-memory"), m)
		clean := filepath.Clean(p)
		for _, file := range rulesManagedFiles {
			r.checkOrphanedRulesFile(filepath.Join(p, file), clean, m)
		}
	}
}

// ledgered reports whether path is present in a ledger's path list — exact
// string match, mirroring how checkPreservedEntries compares ledger entries.
func ledgered(paths []string, path string) bool {
	for _, p := range paths {
		if p == path {
			return true
		}
	}
	return false
}

// maxDoctorReadBytes bounds every file doctor reads while scanning for
// markers or orphaned blocks — the same defensive read-cap style the v1.18.0
// audit applied everywhere else this repo reads content it does not fully
// control (1 MiB via io.LimitReader/http.MaxBytesReader:
// auth.maxDiscoveryBodyBytes, govern.llmMaxRespBytes, api.maxRequestBodyBytes
// are all "1 << 20"). Without it, a huge file at one of these paths is read
// entirely into memory just to look for a marker near the top of it.
const maxDoctorReadBytes = 1 << 20

// readCapped reads path defensively for a scan that must never hang or blow
// out memory on a file it does not control:
//   - it refuses anything that is not a regular file. os.Stat on a FIFO
//     returns immediately, but OPENING one for reading blocks until a writer
//     attaches — from the caller's side that is indistinguishable from a
//     hang — so the guard has to run before the open, not after it.
//   - it never reads more than maxDoctorReadBytes, via io.LimitReader capped
//     one byte past the limit so a file that is EXACTLY that size is not
//     mistaken for one that overflowed it.
//
// ok=false with a nil error means "skipped: too large to scan safely" — the
// caller must not report content-based findings (e.g. marker health) for
// this read, only that it didn't happen. That is distinct from a non-nil
// error, which callers still report as unreadable.
func readCapped(path string) (data []byte, ok bool, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, false, fmt.Errorf("not a regular file (mode %s)", st.Mode())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	data, err = io.ReadAll(io.LimitReader(f, maxDoctorReadBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxDoctorReadBytes {
		return nil, false, nil
	}
	return data, true, nil
}

// checkOrphanedSeedsUnder scans one agent-memory root and reports every
// managed block found whose (name, path) is not in the manifest's Seeds
// ledger.
//
// scanSeedFiles finds CANDIDATE files by directory shape alone — it does not
// know a block is actually present — so a file it returns may carry no
// ORBEAT-SEED marker at all (an unrelated MEMORY.md), which seedNamesIn
// correctly reports as zero names and this loop skips silently.
//
// A candidate whose block is BOTH unledgered and malformed is reported under
// BOTH CheckOrphanedBlock and CheckMarkers. checkMarkers' own seed loop walks
// the Seeds ledger to find its candidates, so an unledgered file never
// reaches it — this is the only place such a file's marker health is ever
// checked. The two hazards are independent: repairing the markers does not
// make the block tracked, and adding it to the ledger does not make a
// malformed marker safe to splice.
func (r *Report) checkOrphanedSeedsUnder(root string, m manifest) {
	for _, path := range scanSeedFiles(root) {
		data, ok, err := readCapped(path)
		if err != nil || !ok {
			continue // unreadable or too large: nothing more to say than checkMarkers already would for a ledgered file
		}
		content := string(data)
		seen := map[string]bool{}
		for _, name := range seedNamesIn(content) {
			if seen[name] {
				continue
			}
			seen[name] = true
			if ledgered(m.Seeds[name], path) {
				continue
			}
			r.add(Finding{
				Check: CheckOrphanedBlock, Severity: SeverityProblem, Path: path,
				Detail: fmt.Sprintf("carries an ORBEAT-SEED block for %q that the sync ledger does not list — the strip pass works from the ledger, so nothing will ever remove this block on its own", name),
				Remedy: "strip the block by hand, or re-entitle the subagent and run 'orbeat-sync sync' so the ledger picks it up again. doctor only scans REGISTERED projects, so a de-registered project's orphaned blocks stay invisible here — no finding from this check is not proof none exist",
			})
			if !seedMarkersHealthy(content, name) {
				r.add(Finding{
					Check: CheckMarkers, Severity: SeverityProblem, Path: path,
					Detail: fmt.Sprintf("has a malformed ORBEAT-SEED marker (orphan or duplicate) for %q — invisible to the ledger-driven marker check because this block is not ledgered either", name),
					Remedy: "orbeat-sync will keep skipping this block on every run until the marker is repaired by hand",
				})
			}
		}
	}
}

// checkOrphanedRulesFile reports path (one of a project's two rules-managed
// files) as orphaned if it carries a HEALTHY (rulesMarkersHealthy) ORBEAT-RULES
// block and projectRoot is absent from the manifest's Rules ledger.
//
// A bare BEGIN-marker regex match is NOT proof a spliceable block exists:
// rulesBeginRe/rulesEndRe are unanchored substring matches, so an AGENTS.md
// that merely DOCUMENTS the marker syntax in prose (a team README-style line
// explaining what the delimiters look like) matches them too, with no block
// behind it at all. Judging presence on that match alone reported such prose
// as an orphan with a "strip it by hand" remedy — an operator following that
// advice deletes their own documentation. A malformed/duplicate marker is
// likewise left to checkMarkers rather than double-reported here:
// rulesMarkersHealthy's check (checkMarkers) is unconditional over every
// registered project regardless of the ledger, so it already tells the
// operator sync refuses to touch this exact file — the accurate, actionable
// diagnosis for that case, and narrowing here loses no real signal.
func (r *Report) checkOrphanedRulesFile(path, projectRoot string, m manifest) {
	data, ok, err := readCapped(path)
	if err != nil || !ok {
		return // missing/unreadable/too large: nothing to find, or checkMarkers already reports it
	}
	content := string(data)
	if !rulesBeginRe.MatchString(content) {
		return // no managed block present
	}
	if !rulesMarkersHealthy(content) {
		return // malformed/duplicate marker, or prose merely mentioning one — checkMarkers already reports this file
	}
	if ledgered(m.Rules, projectRoot) {
		return
	}
	r.add(Finding{
		Check: CheckOrphanedBlock, Severity: SeverityProblem, Path: path,
		Detail: "carries an ORBEAT-RULES block for a project the sync ledger does not list — the strip pass works from the ledger, so nothing will ever remove this block on its own",
		Remedy: "strip the block by hand, or re-entitle a rule for this project and run 'orbeat-sync sync' so the ledger picks it up again. doctor only scans REGISTERED projects, so a de-registered project's orphaned blocks stay invisible here — no finding from this check is not proof none exist",
	})
}

// checkMarkers inspects every ORBEAT-managed file doctor can name directly —
// each registered project's AGENTS.md/CLAUDE.md for rules, and every ledgered
// seed's MEMORY.md — for a malformed sentinel marker (an orphan or duplicate
// BEGIN/END). Sync refuses to touch such a file on every run, silently — the
// refusal is a warning in the middle of a run nobody reads twice.
//
// This does NOT fs-scan for managed blocks the ledger has forgotten about —
// that is orphan detection, a different check with a different job. Seed
// candidates here come from the manifest's Seeds ledger (mirroring
// checkPreservedEntries' shape-validated ledger walk), not a filesystem scan.
//
// A file doctor cannot even read is itself reported, not silently skipped —
// an unreadable file is exactly what an operator needs told about.
func (r *Report) checkMarkers(projects []string, m manifest, err error) {
	for _, p := range projects {
		for _, file := range rulesManagedFiles {
			r.checkRulesMarkerFile(filepath.Join(p, file))
		}
	}

	if err != nil {
		return // surfaced separately by checkManifest
	}
	for name, paths := range m.Seeds {
		for _, p := range paths {
			if !validSeedPath(p) {
				// Not fatal, and not this check's business to explain
				// reachability (checkPreservedEntries owns that) — but
				// silently skipping meant nothing ever reported that doctor
				// is refusing to touch this entry's file at all, by design,
				// because its shape fails the same defense-in-depth guard the
				// reconcilers themselves apply before touching untrusted
				// ledger input.
				r.add(Finding{
					Check: CheckManifest, Severity: SeverityNote, Path: p,
					Detail: fmt.Sprintf("the sync ledger's seed entry %q for %q is not a valid per-subagent memory path — doctor will not read it, and the next 'orbeat-sync sync' will silently drop it from the ledger", p, name),
					Remedy: "no action needed — a malformed entry self-heals on the next sync; if this subagent's memory stopped syncing for a project you expect, run 'orbeat-sync sync'",
				})
				continue
			}
			r.checkSeedMarkerFile(p, name)
		}
	}
}

func (r *Report) checkRulesMarkerFile(path string) {
	data, ok, err := readCapped(path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		r.add(Finding{
			Check: CheckMarkers, Severity: SeverityProblem, Path: path,
			Detail: fmt.Sprintf("cannot be read: %v", err),
			Remedy: "check the file's permissions and ownership",
		})
		return
	}
	if !ok {
		r.add(Finding{
			Check: CheckMarkers, Severity: SeverityNote, Path: path,
			Detail: fmt.Sprintf("exceeds %d bytes — doctor does not scan a file this large, so its markers were not checked", maxDoctorReadBytes),
			Remedy: "no action needed for doctor's sake; if you suspect a malformed ORBEAT-RULES marker here, inspect the file by hand",
		})
		return
	}
	if !rulesMarkersHealthy(string(data)) {
		r.add(Finding{
			Check: CheckMarkers, Severity: SeverityProblem, Path: path,
			Detail: "has a malformed ORBEAT-RULES marker (orphan or duplicate BEGIN/END)",
			Remedy: "orbeat-sync will keep skipping this file on every run until the marker is repaired by hand",
		})
	}
}

func (r *Report) checkSeedMarkerFile(path, name string) {
	data, ok, err := readCapped(path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		r.add(Finding{
			Check: CheckMarkers, Severity: SeverityProblem, Path: path,
			Detail: fmt.Sprintf("cannot be read: %v", err),
			Remedy: "check the file's permissions and ownership",
		})
		return
	}
	if !ok {
		r.add(Finding{
			Check: CheckMarkers, Severity: SeverityNote, Path: path,
			Detail: fmt.Sprintf("exceeds %d bytes — doctor does not scan a file this large, so its markers for %q were not checked", maxDoctorReadBytes, name),
			Remedy: "no action needed for doctor's sake; if you suspect a malformed ORBEAT-SEED marker here, inspect the file by hand",
		})
		return
	}
	if !seedMarkersHealthy(string(data), name) {
		r.add(Finding{
			Check: CheckMarkers, Severity: SeverityProblem, Path: path,
			Detail: fmt.Sprintf("has a malformed ORBEAT-SEED marker (orphan or duplicate) for %q", name),
			Remedy: "orbeat-sync will keep skipping this block on every run until the marker is repaired by hand",
		})
	}
}

// checkManifest flags a sync manifest that exists but will not parse.
// loadManifest returns a nil error for BOTH an absent sync root and a sync
// root with no manifest file yet — a machine that has never synced — so this
// check must NOT fire on either of those; checkSyncRoot already covers the
// former. Only a manifest that exists and fails to load is a problem here: it
// makes sync abort outright (exit 2, a fatalError) rather than silently
// working around state it cannot trust.
//
// A root checkSyncRoot has already flagged (unreadable, or not a directory at
// all) is skipped here entirely (B3): loadManifest's error in that case is
// really "couldn't open the sync root", not "couldn't parse the manifest" —
// surfacing it as a manifest finding blames the wrong thing and, for a
// not-a-directory root, would even name an impossible path
// (<file>/.orbeat-sync-manifest.json). One cause, one finding, with the
// remedy that fits the actual fault. Diagnose runs checkSyncRoot before this,
// so its finding — if any — is already in r.Findings by the time this runs.
func (r *Report) checkManifest(claudeDir string, err error) {
	for _, f := range r.Findings {
		if f.Check == CheckSyncRoot {
			return
		}
	}
	if err != nil {
		r.add(Finding{
			Check: CheckManifest, Severity: SeverityProblem, Path: filepath.Join(claudeDir, manifestName),
			Detail: fmt.Sprintf("the sync manifest cannot be read or parsed: %v", err),
			Remedy: "if you have a backup, restore it; otherwise remove the manifest and run 'orbeat-sync sync' — it will rebuild it and re-fetch your entitled artifacts",
		})
	}
}

// checkAuth is unconditional: unlike every other check, it reads nothing from
// disk and fires on every Diagnose call, healthy tree or not. doctor stays
// offline by design (see the Diagnose doc comment), so it cannot itself tell
// a valid session from an expired one — 'orbeat-sync status' already does,
// distinguishing not-logged-in, valid, and expired with or without a usable
// refresh token. Duplicating that here would risk a second implementation
// drifting from the first, which is worse than a diagnosis that stays
// silent about auth: at least a silent doctor cannot actively mislead.
//
// Always SeverityNote — never Problem. A user whose actual problem is auth
// gets pointed at 'status' on every run, not only when some unrelated check
// also happens to fire; but that pointer must never itself turn a healthy
// tree into one Report.Problems() calls non-zero (see the Severity doc
// comment and CheckAuth's own comment).
func (r *Report) checkAuth() {
	r.add(Finding{
		Check: CheckAuth, Severity: SeverityNote,
		Detail: "doctor does not check authentication — it stays offline by design, so an expired or missing token is invisible to it",
		Remedy: "run 'orbeat-sync status' to check whether you're logged in and your session is valid",
	})
}
