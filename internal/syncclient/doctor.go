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
	// root) is absent from the corresponding manifest ledger.
	//
	// The two halves carry DIFFERENT severities, because the two strip passes
	// are not built the same way. RULES is ledger-driven (rules.go's strip
	// walks m.Rules and nothing else), so an unlisted ORBEAT-RULES block is
	// invisible to every future sync and nothing will ever remove it:
	// SeverityProblem. SEEDS is not (seed.go's candidate set is the UNION of
	// the ledger and a filesystem scan of the very agent-memory roots this
	// check scans), so an unlisted ORBEAT-SEED block is already a candidate
	// of the next ordinary sync: SeverityNote. One claim used to cover both;
	// it was true for rules, false for seeds, and it sent operators to
	// hand-edit a file that fixes itself.
	//
	// doctor can only discover either in REGISTERED projects. A de-registered
	// project's orphans need a full filesystem scan doctor does not perform,
	// so an empty result here is not proof there are none; each finding says
	// so in its own Remedy. That gap is narrower than it once was, but only
	// for a block this client recorded writing: CheckStrandedEntry walks the
	// Seeds ledger itself, so a seed block in an unregistered project is
	// reported when the ledger still names it. A block no ledger names stays
	// invisible, which is the case this paragraph is about.
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
	//     traversal guard, validManagedFilePath, validRulesPath,
	//     validSeedPath). A traversing Files entry is itself fatal (Reconcile
	//     returns the identical error and every sync aborts at exit 2), so it
	//     is a SeverityProblem too. A shape-invalid Files/Rules/Seeds entry is
	//     not fatal: the owning reconciler warns (or says nothing) and drops it
	//     from the ledger on its own next run, so it is a SeverityNote.
	CheckManifest Check = "manifest"
	// CheckInstall flags an install.json this client cannot read: the file
	// holding the random uuid that names this machine in deployment reports.
	// It is a PROBLEM rather than a note because of what the alternative
	// repair would be. EnsureInstallID refuses to write over a file it cannot
	// parse, so reporting stops until a human looks; if it regenerated
	// instead, this machine would start a SECOND identity and the server
	// would count it as a second install for as long as retention keeps the
	// first. An ABSENT install.json is not a finding at all: the id is
	// created on the first report, so its absence is the normal state of a
	// machine that has never reported, or that faces a server which does not
	// record deployments.
	CheckInstall Check = "install"
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
	// CheckPins flags a pins.json this client cannot trust: one that will not
	// parse, or one holding a pin at a revision below 1. insertRevision
	// numbers revisions from 1, so nothing valid can carry a smaller one
	// (parsePins' own sentinel, internal/api/sync_pins.go). Everything else
	// about a pin needs the server: whether it names an artifact this caller
	// is still entitled to, whether the server honours it or clamps it,
	// which is exactly what doctor cannot ask (see the Diagnose doc
	// comment), so this check covers only what is verifiable offline. An
	// ABSENT pins.json is not a finding: a machine that has never run
	// 'orbeat-sync pin' has none.
	CheckPins Check = "pins"
	// CheckStrandedEntry flags a Seeds ledger entry whose MEMORY.md lies
	// under neither the sync root nor any registered project. Since the seed
	// strip pass started choosing each candidate's containment boundary from
	// the roots the RUN was handed (trustedSeedBoundary, seed.go) rather than
	// deriving one from the untrusted ledger path, such an entry is skipped
	// and preserved on every run: the governed block is stranded, and no
	// sync will ever remove it.
	//
	// Always SeverityProblem, and the contrast with CheckOrphanedBlock's seed
	// half is the whole reason both exist. An orphaned block sits UNDER a
	// scanned root, so the next ordinary sync already sees it and it
	// self-heals, which is why that one is a note. A stranded block sits
	// outside every root, so nothing self-heals and a human has to name the
	// path before anything can touch it.
	CheckStrandedEntry Check = "stranded-entry"
	// CheckProjectsFile flags a registered-projects file (projects.json) entry
	// that fails validProjectPath's shape check (not absolute, not already
	// clean, or empty) — the same guard AddProject's own write path always
	// satisfies, so a failing entry can only be hand-edited or corrupted
	// (B25's own reasoning, projects.go). LoadProjects silently drops such an
	// entry rather than erroring, so the project it names simply stops
	// syncing with nothing anywhere telling the developer why; this check
	// exists to close that visibility gap.
	//
	// Always SeverityProblem, deliberately NOT the self-healing pattern
	// CheckManifest uses for a malformed Rules/Seeds ledger entry. THAT
	// pattern is a note because the owning reconciler warns and drops the
	// bad line from ITS OWN ledger on its next save — the file repairs
	// itself. Nothing repairs projects.json: it is user-authored, and
	// orbeat-sync's only write paths (AddProject/RemoveProject) always
	// resave the list LoadProjects just handed them, which already excludes
	// this entry — so the very next 'project add' or 'project remove' call
	// silently erases the malformed line from disk, taking with it whatever
	// the developer meant by it, without ever having surfaced the mistake.
	// Calling that a note, or a "self-heals" remedy, would repeat the A9
	// error the other direction: this is the one case where doing nothing IS
	// the damage.
	//
	// An ABSENT projects.json is not a finding, mirroring CheckPins and
	// CheckInstall: a machine that has never registered a project has none.
	CheckProjectsFile Check = "projects-file"
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
//
// installPath is ~/.config/orbeat/install.json (DefaultInstallPath), pinsPath
// is ~/.config/orbeat/pins.json (DefaultPinsPath), and projectsPath is
// ~/.config/orbeat/projects.json (DefaultProjectsPath) — all passed in like
// every other path this function inspects rather than resolved here: an
// offline diagnosis that derived its own home directory could not be pointed
// at a fixture. A path that resolves to nothing reads as an absent file, which
// is not a finding.
//
// projects is already the FILTERED output of LoadProjects(projectsPath) (via
// ProjectPaths) — every check above CheckProjectsFile works from that
// already-valid list, exactly as before this parameter was added. Only
// checkProjectsFile itself reads projectsPath directly, because it is the one
// check whose entire job is to see what LoadProjects's own filter throws away.
func Diagnose(claudeDir string, projects []string, installPath, pinsPath, projectsPath string) Report {
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

	// Runs BEFORE checkPreservedEntries, which consults its findings to avoid
	// contradicting them for the same path.
	r.checkStrandedEntries(claudeDir, projects, m, err)
	r.checkPreservedEntries(claudeDir, m, err)
	r.checkLedgerDrift(claudeDir, projects, m, err)
	r.checkOrphanedBlocks(claudeDir, projects, m, err)
	r.checkMarkers(projects, m, err)
	r.checkManifest(claudeDir, err)
	r.checkInstall(installPath)
	r.checkPins(pinsPath)
	r.checkProjectsFile(projectsPath)
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

// checkStrandedEntries walks the manifest's Seeds ledger and reports every
// entry the seed strip pass will refuse to touch: one lying under neither the
// sync root nor any registered project.
//
// This is the only check here whose subject set comes from the ledger rather
// than from a filesystem walk, and that is exactly why it exists. Every
// block-finding check next door scans <claudeDir>/agent-memory and each
// registered project's .claude/agent-memory, which are the same roots
// ReconcileSeeds now trusts, so a block outside them is invisible to doctor
// for precisely the reason it is untouchable by sync. The ledger is the only
// record left that names it.
//
// The trusted set is built the way seed.go builds it and matched with
// seed.go's own trustedSeedBoundary, so this reports exactly the entries that
// pass skips, never a set derived independently that could drift from it.
//
// RULES AND GLOBALS ARE NOT WALKED HERE, and that is now a real gap rather
// than a non-issue (B23 audit finding, closed in rules.go/rules_global.go):
// ReconcileRules and reconcileGlobalRules used to open their containment
// roots directly AT the ledgered path (a project root, or a global file's own
// directory) with no membership check against the registered/known-target
// set, so an entry for a de-registered project (or an untrusted global
// directory) was still silently stripped by an ordinary sync — the same
// escape trustedSeedBoundary closed for seeds, just left open on these two
// paths. Both now consult a trusted set (trustedProjects in rules.go,
// allGlobalRuleTargets in rules_global.go) exactly the way ReconcileSeeds
// consults trustedSeedBoundary, and an untrusted entry is preserved+warned
// rather than stripped — which means a Rules or Globals block CAN now strand
// exactly like a Seeds one can, and TestRulesLedgerEntriesDoNotStrand (which
// used to pin the opposite) was rewritten to prove that rather than disprove
// it. Extending checkStrandedEntries to walk the Rules and Globals ledgers
// too would close the remaining visibility gap (a stranded rules/globals
// block currently gets no CheckStrandedEntry finding at all), but that is a
// separate, not-yet-built change — see B23's fix notes.
//
// A ledger entry whose file is absent, or which carries no marker for that
// name, is NOT reported. There is no governed block to strand, and the remedy
// would send an operator to register and de-register a project in order to
// strip nothing. An unreadable or oversized file is left alone here too:
// checkMarkers walks this same ledger and reports it directly, so nothing is
// lost.
//
// Requiring the file is also what keeps this check from contradicting
// checkPreservedEntries, and the reason there is no suppression code between
// them. That check's seed half notes a path whose seedProjectGuess root fails
// os.Stat, with a "no action needed, reconnect and sync self-heals it"
// remedy. A readable file implies every ancestor of it exists, and that guess
// is always an ancestor, so the two can never fire for the same path. A
// suppression written for that pairing would have been dead code, verified by
// probe rather than reasoned about after the fact.
// trustedRoots is the set ReconcileSeeds hands trustedSeedBoundary: the sync
// root plus every registered project. Both checkStrandedEntries and
// checkLedgerDrift ask the same question of it, so it is built once here. Two
// constructions of one set is how the two checks would come to disagree about
// which entries sync can reach, and disagreeing is the whole defect class both
// of them exist to report.
func trustedRoots(claudeDir string, projects []string) []string {
	trusted := make([]string, 0, len(projects)+1)
	trusted = append(trusted, filepath.Clean(claudeDir))
	for _, p := range projects {
		trusted = append(trusted, filepath.Clean(p))
	}
	return trusted
}

func (r *Report) checkStrandedEntries(claudeDir string, projects []string, m manifest, err error) {
	if err != nil {
		return // surfaced separately by checkManifest
	}
	trusted := trustedRoots(claudeDir, projects)

	reported := map[string]bool{} // key: name + "\x00" + path, mirroring seed.go's own
	for name, paths := range m.Seeds {
		for _, p := range paths {
			if !validSeedPath(p) {
				// The strip pass drops a shape-invalid entry on its own, and
				// checkPreservedEntries/checkMarkers already note it. It is
				// not stranded: nothing is holding it.
				continue
			}
			if _, ok := trustedSeedBoundary(trusted, p); ok {
				continue // the strip pass reaches it
			}
			if reported[name+"\x00"+p] {
				continue
			}
			data, ok, readErr := readCapped(p)
			if readErr != nil || !ok {
				continue
			}
			if !seedBeginRe(name).MatchString(string(data)) {
				continue // the ledger line outlived the block; nothing is stranded
			}
			reported[name+"\x00"+p] = true
			r.add(Finding{
				Check: CheckStrandedEntry, Severity: SeverityProblem, Path: p,
				Detail: fmt.Sprintf("carries an ORBEAT-SEED block for %q that the sync ledger tracks, but this path lies under neither the sync root nor any registered project, so every 'orbeat-sync sync' skips it and keeps the entry: nothing will remove this block on its own", name),
				Remedy: strandedSeedRemedy(p),
			})
		}
	}
}

// strandedSeedRemedy names the commands that actually clear a stranded block.
// Registering the containing project is enough by itself, because the entry
// the strip pass preserved makes the next ordinary sync strip the block;
// 'project remove' is named as well because it is the supported way to say
// "stop managing this", and it strips the project's rules in the same pass.
//
// The project root is named only for a path with the project-scope shape,
// <root>/.claude/agent-memory/<name>/MEMORY.md. For any other layout the
// four-directories-up derivation returns an ANCESTOR of the path rather than
// a project (/Users for /Users/bob/agent-memory/x/MEMORY.md, see
// seedProjectGuess), and telling an operator to register that is worse than
// telling her nothing: she would put her whole home directory under sync's
// management to clean up one file.
func strandedSeedRemedy(p string) string {
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(p))))
	if filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p)))) != ".claude" || filepath.Dir(root) == root {
		return "register a directory that contains this path with 'orbeat-sync project add', then 'orbeat-sync project remove' it; or strip the ORBEAT-SEED block by hand"
	}
	return fmt.Sprintf("run 'orbeat-sync project add %s' then 'orbeat-sync project remove %s'; registering it alone is also enough, because the preserved ledger entry makes the next ordinary sync strip the block", root, root)
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

	// A Seeds entry is a MEMORY.md path, not a project root, so the project is
	// guessed from its shape via seedProjectGuess. That guess is a
	// REACHABILITY probe only and never a containment root (see its doc): this
	// check os.Stats the result, and nothing here opens, reads or writes
	// through it. A user-scope seed (guess == claudeDir) is not project-bound,
	// so it is out of scope for this check.
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
			boundary := seedProjectGuess(claudeDir, p)
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

// rebuildManifestRemedy states what deleting the sync manifest actually costs,
// appended to whatever repair step the caller is proposing.
//
// It exists because both places that offered this repair used to end with "it
// will rebuild it and re-fetch your entitled artifacts", and that was measured
// false (audit A9): Reconcile classified every present-but-unledgered file as
// an unmanaged collision, so the rebuilt ledger came back EMPTY and every skill
// and subagent stayed frozen at its old content on that run and on every run
// after, reported only as skipped. Adoption of a byte-identical file fixed the
// common case, and this sentence covers the two parts of the claim that are
// still not true, both re-derived from the code rather than assumed:
//
//   - A rendered file whose content DIFFERS from what the server serves is
//     still an unmanaged collision. It is left alone, reported as skipped, and
//     stays out of the rebuilt ledger, which is deliberate: adopting it would
//     take ownership of content the developer may have written.
//   - The rules and globals strip passes are ledger-only (see the "Unlike
//     seed.go" comment on ReconcileRules' strip pass), so deleting the manifest
//     makes an ORBEAT-RULES block in a project or user-level file that is no
//     longer entitled unstrippable. The seeds ledger is the exception and is
//     not claimed here: its strip pass also scans the managed roots, so a seed
//     block under the sync root or a registered project is still found.
//
// The two callers share this text so the two remedies cannot drift apart, which
// is how they came to state the same false thing in two places.
func rebuildManifestRemedy(step string) string {
	return "if you have a backup, restore it; otherwise " + step +
		". A rebuild is not a restore: sync re-fetches your entitled skills and subagents and adopts the ones already on disk whose content matches, but a rendered file whose content differs is left alone and only reported as skipped, and the rules ledger goes with the manifest, so an ORBEAT-RULES block in a project or user-level file you are no longer entitled to will never be stripped"
}

func (r *Report) checkLedgerDrift(claudeDir string, projects []string, m manifest, err error) {
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
				Remedy: rebuildManifestRemedy("edit the manifest to remove this entry, or delete the manifest entirely and run 'orbeat-sync sync'"),
			})
			continue
		}
		if !validManagedFilePath(rel) {
			// Same treatment the Rules and Seeds ledgers get in
			// checkPreservedEntries, and for the same reason: Reconcile applies
			// this exact check, warns, and drops the entry on its own next run,
			// so the state self-heals and a note is the honest severity.
			//
			// The `continue` is load-bearing, not tidiness. Without it the drift
			// check below would fire for a shape-invalid entry whose file is
			// absent and tell the operator to "run 'orbeat-sync sync' to
			// recreate it", which sync will never do: it drops the entry
			// instead. That remedy became false the moment Reconcile started
			// refusing these entries, and a wrong instruction is worse here
			// than silence.
			r.add(Finding{
				Check: CheckManifest, Severity: SeverityNote, Path: rel,
				// The cause is deliberately left open, the same way Reconcile's
				// own warning leaves it open: this build refuses the entry
				// because validManagedFilePath derives its accepted set from
				// THIS build's fileBackedTypes, and a newer orbeat-sync that
				// manages a further file type writes entries this one has no
				// path function for. Naming a tamper would be a guess.
				Detail: fmt.Sprintf("the sync ledger's file entry %q is not a path this orbeat-sync writes (it renders only skills/<name>/SKILL.md and agents/<name>.md); it was hand-edited, tampered with, or written by a newer orbeat-sync. The next 'orbeat-sync sync' will warn and drop it without touching anything on disk", rel),
				Remedy: "no action needed: the entry self-heals on the next sync, and nothing at that path is removed. If you did not edit the manifest yourself, treat the file it names as something another program pointed orbeat-sync at",
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

	trusted := trustedRoots(claudeDir, projects)
	cleanClaudeDir := filepath.Clean(claudeDir)
	for name, paths := range m.Seeds {
		for _, p := range paths {
			if !validSeedPath(p) {
				continue // checkPreservedEntries/checkMarkers already report a shape-invalid entry
			}
			boundary := seedProjectGuess(claudeDir, p)
			if boundary != cleanClaudeDir {
				if st, err := os.Stat(boundary); err != nil || !st.IsDir() {
					continue // the project itself is unreachable — checkPreservedEntries owns that finding
				}
			}
			if _, err := os.Stat(p); err != nil {
				// "Drops on its own" is true ONLY while the strip pass can
				// reach the path. Under no trusted root ReconcileSeeds skips
				// the entry BEFORE it touches the filesystem, marks it failed
				// and preserves it, so the entry outlives every sync the
				// operator will run. That preservation is correct and is not
				// what this branch changes: skipping before any I/O is exactly
				// why an untrusted path is never touched, which also means the
				// run cannot know the file is gone. What would be wrong is
				// promising a self-correction that cannot happen.
				if _, ok := trustedSeedBoundary(trusted, p); !ok {
					r.add(Finding{
						Check: CheckLedgerDrift, Severity: SeverityNote, Path: p,
						Detail: fmt.Sprintf("the sync ledger tracks a seed for %q here, the file is not on disk, and the path is under neither the sync root nor any registered project, so every future sync skips and preserves this entry rather than clearing it", name),
						Remedy: "nothing is on disk, so nothing is at risk; to clear the ledger line, run 'orbeat-sync project add' on a directory containing that path and sync once",
					})
					continue
				}
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
// manifest ledger does not list. Seeds reuse scanSeedFiles (seed.go), the
// strip pass's own fs-scan, over <claudeDir>/agent-memory and each registered
// project's .claude/agent-memory; rules need no scanner, because they live at
// exactly two known paths per project, AGENTS.md and CLAUDE.md.
//
// Reusing the strip pass's own scanner is exactly why the seed half is a note
// and the rules half is a problem: a seed block found here is by construction
// something the next sync will also find (see CheckOrphanedBlock), while a
// rules block found here is not, because rules never gained an fs-scan.
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
			// A NOTE, not a problem, and the reason is seed.go's candidate
			// set: it unions the ledger with a filesystem scan of the same
			// agent-memory root scanned here, so this block is already a
			// strip candidate of the next ordinary sync. It is still worth
			// reporting rather than dropping, because it is the only signal
			// that a managed block exists which this client did not record
			// writing, and this is also the only place such a file's marker
			// health is ever checked (checkMarkers walks the ledger).
			//
			// A malformed marker changes the remedy but not the severity:
			// sync keeps skipping the block until it is repaired, and the
			// CheckMarkers PROBLEM added just below is what carries that
			// weight. Stating "the next sync removes it" for that case would
			// be the same false claim one layer down.
			healthy := seedMarkersHealthy(content, name)
			remedy := "no action needed: the next 'orbeat-sync sync' strips this block if the subagent is no longer entitled, or records it again if it is. doctor scans REGISTERED projects for blocks the ledger does not list, so no finding from this check is not proof none exist; an unregistered project's block is reported only when the ledger still names it, by the stranded-entry check"
			if !healthy {
				remedy = "repair the marker first (see the marker finding for this file): until then 'orbeat-sync sync' keeps skipping this block instead of stripping it"
			}
			r.add(Finding{
				Check: CheckOrphanedBlock, Severity: SeverityNote, Path: path,
				Detail: fmt.Sprintf("carries an ORBEAT-SEED block for %q that the sync ledger does not list; the seed strip pass unions the ledger with a filesystem scan of this same agent-memory root, so an ordinary sync already sees it", name),
				Remedy: remedy,
			})
			if !healthy {
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
		Remedy: "strip the block by hand, or re-entitle a rule for this project and run 'orbeat-sync sync' so the ledger picks it up again. doctor scans REGISTERED projects for blocks the ledger does not list, so no finding from this check is not proof none exist; a rules ledger entry for an unregistered project is a different case and needs nothing, because the rules strip pass reaches it anyway",
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
			Remedy: rebuildManifestRemedy("remove the manifest and run 'orbeat-sync sync'"),
		})
	}
}

// checkInstall reports an install.json that will not read, and nothing else.
//
// The healthy case is deliberately SILENT: a valid id is not news, and an
// absent file is the normal state of a machine that has never filed a
// deployment report. Only the unreadable case is a finding, and it is a
// PROBLEM, because it is the one state that silently stops reporting while
// every other part of the sync keeps working. LoadInstallID is the same
// function the reporting path calls, so what doctor calls broken is exactly
// what the reporter will refuse.
//
// The Remedy names the cost of the obvious repair rather than just the steps:
// deleting the file does restore reporting, and it also starts a new install
// identity, so this machine's earlier rows linger under the old one until
// retention prunes them. A remedy that hid that would trade a visible problem
// for an invisible wrong number.
func (r *Report) checkInstall(installPath string) {
	if _, err := LoadInstallID(installPath); err != nil {
		r.add(Finding{
			Check: CheckInstall, Severity: SeverityProblem, Path: installPath,
			Detail: fmt.Sprintf("the install identity cannot be read, so this machine cannot file deployment reports: %v", err),
			Remedy: "restore the file if you have a backup; deleting it also works, but the next sync writes a NEW install id and the server keeps this machine's earlier records under the old one until they age out",
		})
	}
}

// checkPins reports a pins.json that will not read, or that holds a pin at a
// revision doctor already knows cannot exist, and nothing else.
//
// The healthy case is deliberately SILENT, mirroring checkInstall: a
// well-formed pin file is not news, and an absent one is the normal state of
// a machine that has never run 'orbeat-sync pin'. Only the two facts
// verifiable without the network are findings, and both are PROBLEMs: a pin
// this client cannot even parse silently stops applying (LoadPins fails, and
// runSync must not guess), and a pin below revision 1 names nothing any
// server could ever serve (insertRevision numbers revisions from 1,
// internal/store/artifact_revision.go), so this can only come from a
// hand-edited file, the same provenance CheckInstall's own doc comment
// assumes for its unreadable case.
func (r *Report) checkPins(pinsPath string) {
	pins, err := LoadPins(pinsPath)
	if err != nil {
		r.add(Finding{
			Check: CheckPins, Severity: SeverityProblem, Path: pinsPath,
			Detail: fmt.Sprintf("the pin file cannot be read: %v", err),
			Remedy: "restore it from a backup if you have one; otherwise remove it and re-pin with 'orbeat-sync pin'",
		})
		return
	}
	for _, p := range pins {
		if p.Revision < 1 {
			r.add(Finding{
				Check: CheckPins, Severity: SeverityProblem, Path: pinsPath,
				Detail: fmt.Sprintf("the pin for %s/%s names revision %d, which no artifact can ever have", p.Type, p.Name, p.Revision),
				Remedy: fmt.Sprintf("remove the pin ('orbeat-sync pin remove %s/%s') and set it again", p.Type, p.Name),
			})
		}
	}
}

// checkProjectsFile reports every entry in projects.json that fails
// validProjectPath's shape check, via loadProjectsWithInvalid — LoadProjects's
// own introspective twin, so this reports exactly the entries the ordinary
// load path silently drops, never a set derived independently that could
// drift from it.
//
// The healthy and the absent cases are both SILENT, mirroring checkPins and
// checkInstall: a machine that has never registered a project has no
// projects.json, and a well-formed one is not news. A file that fails to
// PARSE AT ALL (not merely one bad entry, the whole JSON is invalid) is not
// reported here either — every current caller of LoadProjects (sync, doctor,
// project list) already surfaces that failure loudly by returning the error
// before Diagnose is ever called, unlike a dropped entry, which is silent by
// construction; see loadProjectsWithInvalid's own doc comment. Recording
// nothing here for that case is not a gap this check exists to close.
func (r *Report) checkProjectsFile(projectsPath string) {
	_, invalid, err := loadProjectsWithInvalid(projectsPath)
	if err != nil {
		// REPORTED, not swallowed. An absent file already returns nil error
		// (loadProjectsWithInvalid treats it as "no projects"), so reaching
		// here means the file exists and cannot be read or parsed, which is
		// the one state where every OTHER check in this Report is running
		// against an empty project list that is empty for the wrong reason.
		// Saying so is what stops "0 problems" reading as "your projects are
		// fine".
		r.add(Finding{
			Check: CheckProjectsFile, Severity: SeverityProblem, Path: projectsPath,
			Detail: fmt.Sprintf("the registered-projects file %s exists but cannot be read or parsed (%v) — every project-related check in this diagnosis ran against an EMPTY project list, so their silence means nothing", projectsPath, err),
			Remedy: "fix the file's JSON by hand, or move it aside and re-register each project with 'orbeat-sync project add <path>'",
		})
		return
	}
	for _, raw := range invalid {
		r.add(Finding{
			Check: CheckProjectsFile, Severity: SeverityProblem, Path: raw,
			Detail: fmt.Sprintf("the registered-projects file %s has an entry %q that is not a shape-valid absolute path — orbeat-sync silently ignores it on every sync, and the next 'orbeat-sync project add' or 'project remove' will silently drop this line from the file entirely on its next save", projectsPath, raw),
			Remedy: "edit the file by hand to fix this entry (it must be an absolute, already-clean path) or delete the line; if it named a real project, re-register it with 'orbeat-sync project add <path>'",
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
