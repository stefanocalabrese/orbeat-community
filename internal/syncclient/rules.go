package syncclient

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Governed cross-tool rules (Slice B): orbeat-sync owns exactly one
// ORBEAT-RULES sentinel block per registered project's AGENTS.md (the rules
// content) and its CLAUDE.md (an @AGENTS.md import). Everything outside the
// markers belongs to the developer and is never touched.

const rulesClaudeImport = "@AGENTS.md"

func rulesHash(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(body, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// renderRulesBlock produces the managed block, trailing-newline-terminated.
func renderRulesBlock(body string) string {
	return fmt.Sprintf(
		"<!-- ORBEAT-RULES:BEGIN sha=%s — managed by orbeat-sync; edit OUTSIDE this block -->\n%s\n<!-- ORBEAT-RULES:END -->\n",
		rulesHash(body), strings.TrimRight(body, "\n"))
}

// rulesBlockRe matches the whole managed block, capturing the hash in group 1
// and the block BODY that hash attests in group 2. Group 2 exists so mergeRules
// can re-hash what is actually on disk; it runs to the END marker and so
// includes the newline renderRulesBlock puts between body and marker, which
// rulesHash trims, making rulesHash(group 2) equal rulesHash(what was written).
var rulesBlockRe = regexp.MustCompile(`(?s)<!-- ORBEAT-RULES:BEGIN sha=([0-9a-f]{12}) [^\n]*\n(.*?)<!-- ORBEAT-RULES:END -->\n?`)

var (
	rulesBeginRe = regexp.MustCompile(`<!-- ORBEAT-RULES:BEGIN `)
	rulesEndRe   = regexp.MustCompile(`<!-- ORBEAT-RULES:END -->`)
)

// rulesMarkersHealthy reports whether existing has a clean marker state — at
// most one well-formed block and no orphan/duplicate markers. An orphan BEGIN
// (or a hand-copied duplicate) can make rulesBlockRe span developer content on
// an in-place edit, so a caller must NOT splice such a file — it skips + warns.
func rulesMarkersHealthy(existing string) bool {
	begins := len(rulesBeginRe.FindAllString(existing, -1))
	ends := len(rulesEndRe.FindAllString(existing, -1))
	blocks := len(rulesBlockRe.FindAllString(existing, -1))
	return blocks <= 1 && begins == blocks && ends == blocks
}

// mergeRules sets the single ORBEAT-RULES block in existing to body: updated in
// place if present (idempotent when the BEGIN-marker hash already matches AND
// the body under it still hashes to that marker), appended after the dev's
// content if absent. Only the managed block is touched.
//
// restored reports that the block on disk did not hash to the sha in its own
// BEGIN marker, so this merge overwrote an edited governed body. It is A8, and
// mergeSeed's doc comment carries the full argument; the mechanism, the failure
// it closes and the "restored implies changed" invariant are identical here.
func mergeRules(existing, body string) (merged string, changed bool, restored bool) {
	block := renderRulesBlock(body)
	if loc := rulesBlockRe.FindStringSubmatchIndex(existing); loc != nil {
		markerHash := existing[loc[2]:loc[3]]
		restored = rulesHash(existing[loc[4]:loc[5]]) != markerHash
		if !restored && markerHash == rulesHash(body) {
			return existing, false, false
		}
		return existing[:loc[0]] + block + existing[loc[1]:], true, restored
	}
	trimmed := strings.TrimRight(existing, "\n")
	if trimmed == "" {
		return block, true, false
	}
	return trimmed + "\n\n" + block, true, false
}

// stripRules removes every ORBEAT-RULES block, preserving the dev's content and
// never deleting the file. Returns the number of blocks removed.
func stripRules(existing string) (string, int) {
	n := 0
	for {
		loc := rulesBlockRe.FindStringIndex(existing)
		if loc == nil {
			break
		}
		existing = existing[:loc[0]] + existing[loc[1]:]
		n++
	}
	if n > 0 {
		existing = strings.TrimRight(existing, "\n")
		if existing != "" {
			existing += "\n"
		}
	}
	return existing, n
}

// RulesResult summarizes the rules pass of a sync run.
type RulesResult struct {
	Written, Unchanged, Stripped int
	Warnings                     []string
	Failures                     []string // projects (or the manifest save) that should have synced but did not (non-fatal I/O)
	// Applied names the rule artifacts whose aggregated ORBEAT-RULES block is
	// on disk in at least one registered project after this run: written, or
	// already carrying the desired body.
	//
	// One entry per ARTIFACT, never per project. A per-project entry would put
	// the developer's project directory names into whatever consumes this, and
	// there is deliberately no field here to carry them. The cost, stated: a
	// consumer learns that a rule landed somewhere, never which project got it.
	//
	// It stays empty for a developer with no live registered projects, who is
	// entitled to rules that reached nothing: the served set and the applied
	// set diverge there with no failure and no warning anywhere in this result.
	Applied []AppliedArtifact
}

// renderRulesBody concatenates entitled rules as `## <name>` sections, ordered
// by name for a deterministic (diff-stable) block.
func renderRulesBody(rules []Artifact) string {
	var b strings.Builder
	for i, r := range rules {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## ")
		b.WriteString(r.Name)
		b.WriteString("\n\n")
		b.WriteString(strings.TrimRight(r.Content, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// ReconcileRules writes the aggregated ORBEAT-RULES block into each live
// registered project's AGENTS.md (+ CLAUDE.md @AGENTS.md import), then strips
// the block from every previously-written project no longer desired. The
// manifest Rules ledger records the project roots written to.
//
// A nil error does NOT mean every project synced: a per-project write/strip
// I/O failure, and a project this run could not even stat, are non-fatal —
// each is recorded in RulesResult.Failures (the stat case as a Warning
// instead) and the run continues, isolating one broken project from starving
// the rest. A non-nil error means a whole-sync abort (an unsafe artifact
// name, or a corrupt/unreadable manifest — see fatalError/markFatal/isFatal).
//
// A project that failed this run — including one this run could not stat, an
// unmounted volume being indistinguishable from a deleted directory — keeps a
// ledger entry UNCONDITIONALLY, even on a first-ever sync with no prior
// entry: writeRulesToProject is NOT atomic across its two files (AGENTS.md
// lands, then CLAUDE.md may fail), so the block can already be durably on
// disk with no prior entry to "restore". This is load-bearing here: the
// Rules ledger is the SOLE record of which projects carry a block, unlike
// seed.go's ledger which is backstopped by an fs-scan of the managed roots.
// A spurious entry self-heals: the next run's strip finds no block, returns
// (0, nil), and the entry drops out.
//
// writeRulesToProject also isolates its two managed files from EACH OTHER'S
// failures (mirrors stripRulesFromProject's own per-file isolation), and that
// is a different guarantee from the ledger-preservation paragraph above, not
// a restatement of it. A failed STRIP and a failed WRITE differ in kind: a
// block a strip could not remove sits there until something removes it, and
// the ledger entry above is that "something" — without it, nothing will ever
// touch that path again. A block a write could not produce is retried by the
// very next ordinary sync as long as the artifact stays entitled; there is no
// partial-write ledger to consult, because the desired state is recomputed
// from scratch on every run. That difference does NOT make write-side
// isolation optional, though: without it, a single persistently-failing file
// (a permission problem on one of the two paths, say) blocks the OTHER,
// healthy file's write on every retry, not just the first one — so "the next
// sync fixes it" is false for exactly the case isolation exists to help. A
// project can then sit indefinitely with AGENTS.md carrying the rules and
// CLAUDE.md missing the @AGENTS.md import that makes Claude Code read them
// (or the reverse), which is a real, observable half-applied state even
// though the ledger accounting is not at risk the way it is on the strip
// side. Isolation makes each retry make maximal progress instead of none.
// rulesFor returns the rules that apply to a project carrying these tags.
//
// A rule with NO target tags applies everywhere. That is the pre-targeting
// behaviour and it is also what an older server produces by not sending the
// field, so an un-upgraded server and a new client agree without negotiating.
// A rule WITH tags applies when the project carries at least one of them:
// intersection, not containment, so a rule for `go` or `rust` reaches a project
// that is either.
//
// A project with no tags therefore receives untargeted rules only, which is the
// same set it received before it could be tagged. Nothing a developer has to do
// to keep what they already had.
func rulesFor(rules []Artifact, tags []string) []Artifact {
	have := make(map[string]bool, len(tags))
	for _, t := range tags {
		have[t] = true
	}
	out := make([]Artifact, 0, len(rules))
	for _, r := range rules {
		if len(r.TargetTags) == 0 {
			out = append(out, r)
			continue
		}
		for _, t := range r.TargetTags {
			if have[t] {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

func ReconcileRules(claudeDir string, projects []Project, artifacts []Artifact, plan *Plan) (RulesResult, error) {
	var res RulesResult
	m, err := loadManifest(claudeDir)
	if err != nil {
		return res, err
	}

	var rules []Artifact
	for _, a := range artifacts {
		if a.Type != "rule" {
			continue
		}
		if !artifactNameRe.MatchString(a.Name) {
			return res, markFatal(fmt.Errorf("rules: unsafe artifact name %q", a.Name))
		}
		rules = append(rules, a)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })

	// Split by scope before anything else. The two halves share the rendering
	// and the merge machinery and NOTHING else: a global rule has no project to
	// be targeted at, and a project rule has no business in a file that every
	// project inherits.
	var globalRules []Artifact
	projectRules := rules[:0:0]
	for _, r := range rules {
		if r.RuleScope == "global" {
			globalRules = append(globalRules, r)
			continue
		}
		projectRules = append(projectRules, r)
	}
	rules = projectRules

	live := make([]Project, 0, len(projects))
	notLive := map[string]bool{}
	for _, proj := range projects {
		if st, err := os.Stat(proj.Path); err != nil || !st.IsDir() {
			res.Warnings = append(res.Warnings, fmt.Sprintf("registered project %s missing — skipped", proj.Path))
			notLive[filepath.Clean(proj.Path)] = true
			continue
		}
		live = append(live, proj)
	}

	desired := map[string]bool{}
	failed := map[string]bool{}
	var newLedger []string

	// A registered project we cannot stat may still hold a block (an unmounted
	// volume is indistinguishable from a deleted directory), so treat it as
	// failed: skip the strip pass for it and keep its ledger entry, or a
	// transient outage would strand the block permanently.
	for _, p := range m.Rules {
		clean := filepath.Clean(p)
		if notLive[clean] {
			failed[clean] = true
			newLedger = append(newLedger, clean)
		}
	}

	if len(rules) > 0 {
		// landed records which rules reached at least one project INTACT. It
		// was a single bool before targeting, when every project received the
		// same body; now a rule can land in one project and apply to no other,
		// so the record has to be per rule. It is still not res.Written > 0: a
		// project whose AGENTS.md carries a malformed marker is skipped while
		// its CLAUDE.md import still merges, which counts as Written and
		// applies nothing.
		landed := map[string]bool{}
		for _, proj := range live {
			clean := filepath.Clean(proj.Path)
			// The rules for THIS project. A project matching none gets no
			// write at all, and the strip pass below then removes any block a
			// previous sync left there, which is what makes re-targeting a
			// rule away from a project actually take the block off that
			// developer's disk rather than freezing it.
			matched := rulesFor(rules, proj.Tags)
			if len(matched) == 0 {
				continue
			}
			body := renderRulesBody(matched)
			changed, notes, err := writeRulesToProject(proj.Path, body, plan)
			if err != nil {
				if isFatal(err) {
					return res, err // No fatal source reaches this call today (rel is a literal); the check keeps a future one routed correctly.
				}
				res.Failures = append(res.Failures, fmt.Sprintf("%s: rules: write: %v", proj.Path, err))
				failed[clean] = true
				// Preserve unconditionally: writeRulesToProject is NOT atomic across
				// its two files (AGENTS.md then CLAUDE.md), so a failure may leave the
				// block on disk even on a first-ever sync where there is no prior entry.
				// The rules ledger is the SOLE record — a missed entry orphans the block
				// forever. A spurious entry is self-correcting: the next run's strip finds
				// no block, returns (0, nil), and the entry drops out.
				newLedger = append(newLedger, clean)
				continue
			}
			res.Warnings = append(res.Warnings, notes.all()...)
			if changed {
				res.Written++
			} else {
				res.Unchanged++
			}
			// A project counts as applied only when BOTH managed files ended in
			// the desired state. notes.skips is populated by exactly one thing,
			// mergeRulesFile's malformed-marker skip, so an empty skips slice
			// on a call that returned no error IS that condition, per file, for
			// both files. notes.restored is deliberately NOT consulted: an A8
			// restoration means the desired bytes were just written over an
			// edited body, so the file IS in the desired state and counting it
			// as unapplied would report a governed block as missing at the one
			// moment it had been put back. The unit here is the project, not the file:
			// writeRulesToProject writes AGENTS.md (the rules themselves) and
			// CLAUDE.md (the @AGENTS.md import Claude Code needs to read them),
			// and the caller records one failure per project, not per file.
			// Rules that sit in an AGENTS.md nothing imports are not deployed
			// for every tool, and under-claiming is the safe direction for a
			// record whose question is who is still behind.
			if len(notes.skips) == 0 {
				for _, r := range matched {
					landed[r.ID] = true
				}
			}
			desired[clean] = true
			newLedger = append(newLedger, clean)
		}
		// Ordered by name (rules was sorted above) rather than by whatever
		// order the projects happened to land in, so the report is stable
		// across machines with the same entitlements and different projects.
		for _, r := range rules {
			if landed[r.ID] {
				res.Applied = appendApplied(res.Applied, r.ID, r.Revision)
			}
		}
	}

	if err := reconcileGlobalRules(claudeDir, globalRules, &m, &res, plan); err != nil {
		return res, err
	}

	// trustedProjects is the set THIS RUN was actually handed — every
	// registered project, live or not (a registered-but-currently-unreachable
	// one is already excluded above via `failed`, but is included here too for
	// the same reason seed.go's `trusted` includes them: trust is a property of
	// registration, not of reachability). Mirrors trustedSeedBoundary (B23):
	// a rules ledger entry IS a project root, and the strip pass must never
	// treat the untrusted ledger itself as proof a path is a project — a root
	// derived from (or merely confirmed to exist via os.Stat on) the untrusted
	// path is exactly the construction trustedSeedBoundary's own doc comment
	// says to avoid, and validRulesPath alone (shape only: absolute + clean)
	// cannot tell a de-registered project from one nobody ever registered.
	trustedProjects := make(map[string]bool, len(projects))
	for _, proj := range projects {
		trustedProjects[filepath.Clean(proj.Path)] = true
	}

	// Strip pass: previously-written projects no longer desired. Unlike seed.go
	// (which also fs-scans the managed roots), the rules strip is ledger-only —
	// rules are project-root-scoped, so a lost manifest cannot be reconstructed
	// from `projects` alone; a shape-check plus trustedProjects membership
	// guards each untrusted ledger entry.
	for _, p := range m.Rules {
		clean := filepath.Clean(p)
		if desired[clean] || failed[clean] {
			continue // desired: keep. failed: already recorded + preserved above; don't re-touch or double-report.
		}
		if !validRulesPath(p) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("ignoring malformed rules ledger entry %q", p))
			continue
		}
		if !trustedProjects[clean] {
			// Well-formed but names no project this run was handed — a
			// de-registered project (see B24: RemoveProject's caller must
			// strip BEFORE de-registering, or this is exactly how a block
			// strands) or a tampered manifest entry. PRESERVED, not dropped,
			// for the same v1.15.0 cost-asymmetry argument seed.go's own
			// untrusted-boundary branch makes: this run did not verify the
			// block is gone, and 'orbeat-sync project remove' (or
			// re-registering the project) is the supported way to reach it.
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"sync ledger entry %s names a project that is not currently registered; skipped — orbeat-sync will not touch it (register it again, or run 'orbeat-sync project remove %s', to strip it explicitly)", clean, clean))
			newLedger = append(newLedger, clean)
			continue
		}
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			// S5: an unreachable de-registered project root (an unmounted volume —
			// ENOENT here is indistinguishable from a genuine delete) may still carry
			// the ORBEAT-RULES block. Preserve the ledger entry — generalizing the
			// notLive rule, which only covers REGISTERED projects, to unregistered
			// ones — so a later run strips it once the root is reachable, instead of
			// dropping it now (a clean-strip false positive) and orphaning the block.
			res.Warnings = append(res.Warnings, fmt.Sprintf("project %s is unreachable — its ORBEAT-RULES block (if any) remains; it will be stripped when the path is reachable and a sync runs", p))
			newLedger = append(newLedger, clean)
			continue
		}
		n, warnings, err := stripRulesFromProject(p, plan)
		if err != nil {
			if isFatal(err) {
				return res, err // No fatal source reaches this call today (rel is a literal); the check keeps a future one routed correctly.
			}
			res.Failures = append(res.Failures, fmt.Sprintf("%s: rules: strip: %v", p, err))
			newLedger = append(newLedger, clean) // preserve: block may remain, retry next run
			continue
		}
		res.Stripped += n
		if len(warnings) > 0 {
			res.Warnings = append(res.Warnings, warnings...)
			// Preserve: a malformed marker blocked the splice for at least one
			// of the two managed files, so a block may still be on disk — a
			// later run after manual repair must retry it.
			newLedger = append(newLedger, clean)
		}
	}

	// Dedupe before saving: FIX for a duplicated `projects` input (the write
	// loop would otherwise append the same clean path twice) and for a
	// tampered manifest with duplicate ledger entries feeding the notLive
	// preservation loop above.
	seen := map[string]bool{}
	deduped := make([]string, 0, len(newLedger))
	for _, p := range newLedger {
		if seen[p] {
			continue
		}
		seen[p] = true
		deduped = append(deduped, p)
	}
	newLedger = deduped

	sort.Strings(newLedger)
	m.Rules = newLedger
	if len(m.Rules) == 0 {
		m.Rules = nil
	}
	if err := saveManifest(claudeDir, m, plan); err != nil {
		res.Failures = append(res.Failures, fmt.Sprintf("manifest: %v", err))
	}
	return res, nil
}

// writeRulesToProject merges the AGENTS.md rules block and the CLAUDE.md import
// block under proj. Returns changed if either file changed, plus the two
// per-file note kinds (a malformed-marker skip, and an A8 restoration).
//
// Each managed file is ISOLATED (mirrors stripRulesFromProject's own per-file
// isolation, added first on the strip side): a genuine I/O failure on one
// managed file — via mergeRulesFile's read/write — is collected via
// errors.Join, but does not stop the OTHER managed file from being attempted.
// A malformed marker on one file is not an error here at all — mergeRulesFile
// reports it via notes.skips with a nil error, and the loop already continued
// past that case before this fix too.
//
// Before this fix, a plain I/O error on the FIRST managed file the loop
// reached (AGENTS.md, since the loop below orders it first) returned
// immediately — the second file (CLAUDE.md) was never even attempted. A
// failure on CLAUDE.md itself did not have this problem, because AGENTS.md is
// written first and its success already lands on disk before CLAUDE.md is
// ever reached; the bug was one-directional. The caller (ReconcileRules)
// already treats any non-nil error from this function identically regardless
// of which file failed — see its own "writeRulesToProject is NOT atomic"
// comment on the write pass below — so isolating AGENTS.md's failure from
// CLAUDE.md's attempt changes what actually lands on disk, not how the caller
// accounts for it.
func writeRulesToProject(proj, agentsBody string, plan *Plan) (bool, rulesMergeNotes, error) {
	var notes rulesMergeNotes
	// S2 containment: both managed files are written through a root opened at the
	// project boundary, so an AGENTS.md/CLAUDE.md that is (or sits behind) an
	// escaping symlink fails the write instead of following it out of the repo.
	r, ok, err := openRootedOptionalPlanned(proj, plan)
	if err != nil {
		return false, notes, fmt.Errorf("rules: open project root %s: %w", proj, err)
	}
	if !ok {
		// The caller only reaches here for a project it just stat'd as live, so a
		// vanished root is a TOCTOU race: a plain (non-fatal) error → the caller
		// records a failure and preserves the ledger entry.
		return false, notes, fmt.Errorf("rules: project root %s vanished", proj)
	}
	defer r.Close()

	changed := false
	var errs []error
	for _, spec := range []struct{ file, body string }{
		{"AGENTS.md", agentsBody},
		{"CLAUDE.md", rulesClaudeImport},
	} {
		path, err := resolveContained(proj, spec.file)
		if err != nil {
			// resolveContained's only failure is the fatal path-escape check; spec.file
			// is always one of this function's own literals, so no fatal source
			// reaches this call today — the check keeps a future one routed
			// correctly, aborting immediately rather than joined in with the
			// per-file failures below (an untrustworthy containment root is not
			// something the OTHER file should still be written under).
			if isFatal(err) {
				return changed, notes, err
			}
			errs = append(errs, err)
			continue
		}
		c, fileNotes, err := mergeRulesFile(r, path, spec.body)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		notes.merge(fileNotes)
		changed = changed || c
	}
	return changed, notes, errors.Join(errs...)
}

// rulesMergeNotes separates the two things a rules merge can have to say about
// a file, because the caller's "did this project land?" test consults exactly
// one of them.
//
// skips means the desired block is NOT on disk: a malformed marker made the
// splice unsafe and the file was left alone. restored means the desired block
// IS on disk and got there by overwriting a body that no longer hashed to the
// sha in its own marker (A8). Both are printed to the operator; only skips may
// suppress an applied claim. Routing a restoration through skips would report a
// project as not applied precisely when the governed bytes had just been put
// back, which is the opposite of the truth.
type rulesMergeNotes struct {
	skips    []string
	restored []string
}

func (n *rulesMergeNotes) merge(o rulesMergeNotes) {
	n.skips = append(n.skips, o.skips...)
	n.restored = append(n.restored, o.restored...)
}

// all returns every note, for a caller that only wants to print them.
func (n rulesMergeNotes) all() []string {
	out := make([]string, 0, len(n.skips)+len(n.restored))
	out = append(out, n.skips...)
	return append(out, n.restored...)
}

// mergeRulesFile reads the target through the containment root FIRST (to
// preserve the developer's content), so an escaping-symlink target is refused
// here rather than healed the way a write-only path would be — os.Root.ReadFile
// rejects the escape before any splice.
func mergeRulesFile(r *rooted, path, body string) (changed bool, notes rulesMergeNotes, err error) {
	existing := ""
	if data, e := r.readFile(path); e == nil {
		existing = string(data)
	} else if !os.IsNotExist(e) {
		return false, notes, fmt.Errorf("rules: read %s: %w", path, e)
	}
	if !rulesMarkersHealthy(existing) {
		notes.skips = append(notes.skips, fmt.Sprintf("%s has a malformed ORBEAT-RULES marker (orphan or duplicate) — skipped; repair the block manually", path))
		return false, notes, nil
	}
	merged, ch, restored := mergeRules(existing, body)
	if ch {
		if e := r.writeAtomic(path, []byte(merged), r.existingPerm(path, 0o644)); e != nil {
			return false, rulesMergeNotes{}, fmt.Errorf("rules: %w", e)
		}
	}
	// Recorded after the write for symmetry with seed.go, but on THIS path the
	// ordering is NOT what makes a failed write claim no restoration, and saying
	// it is would be a false rationale attached to correct code. Measured:
	// hoisting this block above the write leaves the whole suite green, because
	// every error return between here and RulesResult drops the notes anyway:
	// on ITS OWN write failure this function returns the literal
	// rulesMergeNotes{}, not whatever restored/skips it had accumulated locally
	// (still true after writeRulesToProject's per-file isolation — that change
	// lets the OTHER managed file's mergeRulesFile call still run, it does not
	// change what THIS call returns on ITS OWN error); writeRulesToProject only
	// ever notes.merge()s what mergeRulesFile actually returned for a given
	// file, so a failing file contributes zero notes whether or not the other
	// file's attempt is isolated from it; and ReconcileRules `continue`s past
	// its res.Warnings append on ANY non-nil error from writeRulesToProject,
	// discarding whatever it did return. The property is gated end to end by
	// TestAFailedWriteClaimsNoRestoration/rules, which is what should fail if
	// any of those layers ever changes.
	//
	// The wording stops at the mismatch, for the reason seed.go spells out: this
	// client re-hashes the body only, so an edited sha in the BEGIN marker is
	// indistinguishable from an edited body.
	if restored {
		notes.restored = append(notes.restored, fmt.Sprintf(
			"the ORBEAT-RULES block in %s no longer matches the sha in its own marker, so the block was edited after orbeat-sync wrote it (the body, the marker line, or both); orbeat-sync restored the governed content", path))
	}
	return ch, notes, nil
}

// rulesManagedFiles are the two files orbeat-sync writes an ORBEAT-RULES block
// into per project (AGENTS.md holds the rules, CLAUDE.md the @AGENTS.md import).
var rulesManagedFiles = []string{"AGENTS.md", "CLAUDE.md"}

// validRulesPath rejects a tampered ledger entry before the strip pass touches
// it — defense-in-depth mirroring seed.go's validSeedPath. Rules ledger entries
// are project ROOTS, so require an absolute, already-clean path.
func validRulesPath(p string) bool {
	return filepath.IsAbs(p) && p == filepath.Clean(p)
}

// stripRulesFromProject strips both managed files under proj. Each file is
// ISOLATED (B24, mirroring seed.go's StripProjectSeeds/stripUndesired): a
// malformed marker on one managed file — via stripRulesFile's
// rulesMarkersHealthy check — is reported in the returned warnings, and a
// genuine per-file I/O failure is collected via errors.Join, but neither kind
// of failure on one managed file stops the OTHER from being attempted. A
// caller MUST treat a non-empty warnings slice, or a non-nil returned error,
// as "this file may still carry a block" for ledger-preservation purposes.
//
// Before this fix, a plain I/O error on the first managed file the loop
// reached (AGENTS.md, since rulesManagedFiles orders it first) returned
// immediately — the second file (CLAUDE.md) was never even attempted, and
// StripProjectRules's own ledger update + saveManifest sat above its call to
// this function, so they never ran either. A malformed marker never hit this
// bug because stripRulesFile reports it as a warning, not an error — this
// function's per-file loop already continued past that case.
func stripRulesFromProject(proj string, plan *Plan) (stripped int, warnings []string, err error) {
	// S2 containment: strip through a root opened at proj. A gone project dir
	// (openRootedOptional → !ok) means nothing to strip — same as the old
	// per-file os.ReadFile returning not-exist — so callers drop the ledger entry.
	r, ok, rootErr := openRootedOptionalPlanned(proj, plan)
	if rootErr != nil {
		return 0, nil, fmt.Errorf("rules: open project root %s: %w", proj, rootErr)
	}
	if !ok {
		return 0, nil, nil
	}
	defer r.Close()
	var errs []error
	for _, file := range rulesManagedFiles {
		path, rerr := resolveContained(proj, file)
		if rerr != nil {
			// resolveContained's only failure is the fatal path-escape check; rel
			// is always one of this file's own literals, so no fatal source
			// reaches this call today — the check keeps a future one routed
			// correctly, aborting immediately rather than joined in with the
			// per-file failures below (an untrustworthy containment root is not
			// something the OTHER file should still be stripped under).
			if isFatal(rerr) {
				return stripped, warnings, rerr
			}
			errs = append(errs, rerr)
			continue
		}
		n, warning, serr := stripRulesFile(r, path)
		if serr != nil {
			errs = append(errs, serr)
			continue
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
		stripped += n
	}
	return stripped, warnings, errors.Join(errs...)
}

// stripRulesFile mirrors mergeRulesFile's malformed-marker gate on the strip
// side: stripRules' unconditional regex loop is only safe to run once
// rulesMarkersHealthy confirms there is no orphan/duplicate marker. An orphan
// BEGIN (no matching END) sitting above a LATER genuine block lets
// rulesBlockRe's non-greedy match span from the orphan BEGIN all the way to
// that later block's own END, deleting any developer content in between (and
// possibly the whole file) on an in-place splice. On unhealthy markers the
// file is left byte-for-byte untouched and a warning is returned instead of
// an error — the caller is expected to preserve the ledger entry so a later
// run retries after manual repair.
func stripRulesFile(r *rooted, path string) (n int, warning string, err error) {
	data, err := r.readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, "", nil
		}
		return 0, "", fmt.Errorf("rules: read %s: %w", path, err)
	}
	existing := string(data)
	if !rulesMarkersHealthy(existing) {
		return 0, fmt.Sprintf("%s has a malformed ORBEAT-RULES marker (orphan or duplicate) — skipped; repair the block manually", path), nil
	}
	out, n := stripRules(existing)
	if n == 0 {
		return 0, "", nil
	}
	if err := r.writeAtomic(path, []byte(out), r.existingPerm(path, 0o644)); err != nil {
		return 0, "", fmt.Errorf("rules: %w", err)
	}
	return n, "", nil
}

// StripProjectRules removes both managed blocks under proj and drops it from the
// ledger — the `project remove` semantics (mirrors StripProjectSeeds). proj must
// be the cleaned absolute path.
//
// A malformed marker on either managed file, OR a genuine per-file I/O
// failure stripping it (B24, mirroring StripProjectSeeds), leaves proj's
// ledger entry in place rather than unconditionally dropping it, so a later
// run retries after the file is repaired or made accessible again. Both
// managed files are always attempted regardless of the other's outcome, and
// the manifest is always saved: before this fix, a plain I/O failure on
// either file returned before the ledger update and saveManifest below ever
// ran, so the OTHER managed file's already-stripped, already-on-disk state
// was silently lost from the ledger — not just left un-attempted.
func StripProjectRules(claudeDir, proj string) (int, error) {
	m, err := loadManifest(claudeDir)
	if err != nil {
		return 0, err
	}
	stripped, warnings, stripErr := stripRulesFromProject(proj, nil)
	if stripErr != nil && isFatal(stripErr) {
		// No fatal source reaches stripRulesFromProject today (resolveContained's
		// rel is always a literal); the check keeps a future one routed
		// correctly — a fatal error means the local managed state can no longer
		// be trusted, so the ledger/manifest below must not be touched at all.
		return stripped, stripErr
	}
	var errs []error
	if stripErr != nil {
		errs = append(errs, stripErr)
	}
	keep := stripErr != nil || len(warnings) > 0
	clean := filepath.Clean(proj)
	kept := make([]string, 0, len(m.Rules))
	for _, p := range m.Rules {
		if filepath.Clean(p) == clean {
			if keep {
				kept = append(kept, p)
			}
			continue
		}
		kept = append(kept, p)
	}
	m.Rules = kept
	if len(m.Rules) == 0 {
		m.Rules = nil
	}
	if err := saveManifest(claudeDir, m, nil); err != nil {
		errs = append(errs, err)
	}
	return stripped, errors.Join(errs...)
}
