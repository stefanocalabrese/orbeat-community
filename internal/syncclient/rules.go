package syncclient

import (
	"crypto/sha256"
	"encoding/hex"
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

// rulesBlockRe matches the whole managed block, capturing the hash in group 1.
var rulesBlockRe = regexp.MustCompile(`(?s)<!-- ORBEAT-RULES:BEGIN sha=([0-9a-f]{12}) [^\n]*\n.*?<!-- ORBEAT-RULES:END -->\n?`)

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
// place if present (idempotent when the BEGIN-marker hash already matches),
// appended after the dev's content if absent. Only the managed block is touched.
func mergeRules(existing, body string) (string, bool) {
	block := renderRulesBlock(body)
	if loc := rulesBlockRe.FindStringSubmatchIndex(existing); loc != nil {
		if existing[loc[2]:loc[3]] == rulesHash(body) {
			return existing, false
		}
		return existing[:loc[0]] + block + existing[loc[1]:], true
	}
	trimmed := strings.TrimRight(existing, "\n")
	if trimmed == "" {
		return block, true
	}
	return trimmed + "\n\n" + block, true
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
func ReconcileRules(claudeDir string, projects []string, artifacts []Artifact, plan *Plan) (RulesResult, error) {
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

	live := make([]string, 0, len(projects))
	notLive := map[string]bool{}
	for _, proj := range projects {
		if st, err := os.Stat(proj); err != nil || !st.IsDir() {
			res.Warnings = append(res.Warnings, fmt.Sprintf("registered project %s missing — skipped", proj))
			notLive[filepath.Clean(proj)] = true
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
		body := renderRulesBody(rules)
		for _, proj := range live {
			clean := filepath.Clean(proj)
			changed, warnings, err := writeRulesToProject(proj, body, plan)
			if err != nil {
				if isFatal(err) {
					return res, err // No fatal source reaches this call today (rel is a literal); the check keeps a future one routed correctly.
				}
				res.Failures = append(res.Failures, fmt.Sprintf("%s: rules: write: %v", proj, err))
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
			res.Warnings = append(res.Warnings, warnings...)
			if changed {
				res.Written++
			} else {
				res.Unchanged++
			}
			desired[clean] = true
			newLedger = append(newLedger, clean)
		}
	}

	// Strip pass: previously-written projects no longer desired. Unlike seed.go
	// (which also fs-scans the managed roots), the rules strip is ledger-only —
	// rules are project-root-scoped, so a lost manifest cannot be reconstructed
	// from `projects` alone; a shape-check guards each untrusted ledger entry.
	for _, p := range m.Rules {
		clean := filepath.Clean(p)
		if desired[clean] || failed[clean] {
			continue // desired: keep. failed: already recorded + preserved above; don't re-touch or double-report.
		}
		if !validRulesPath(p) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("ignoring malformed rules ledger entry %q", p))
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
// block under proj. Returns changed if either file changed, plus any per-file
// warnings (a malformed-marker skip).
func writeRulesToProject(proj, agentsBody string, plan *Plan) (bool, []string, error) {
	// S2 containment: both managed files are written through a root opened at the
	// project boundary, so an AGENTS.md/CLAUDE.md that is (or sits behind) an
	// escaping symlink fails the write instead of following it out of the repo.
	r, ok, err := openRootedOptionalPlanned(proj, plan)
	if err != nil {
		return false, nil, fmt.Errorf("rules: open project root %s: %w", proj, err)
	}
	if !ok {
		// The caller only reaches here for a project it just stat'd as live, so a
		// vanished root is a TOCTOU race: a plain (non-fatal) error → the caller
		// records a failure and preserves the ledger entry.
		return false, nil, fmt.Errorf("rules: project root %s vanished", proj)
	}
	defer r.Close()

	changed := false
	var warnings []string
	for _, spec := range []struct{ file, body string }{
		{"AGENTS.md", agentsBody},
		{"CLAUDE.md", rulesClaudeImport},
	} {
		path, err := resolveContained(proj, spec.file)
		if err != nil {
			return changed, warnings, err
		}
		c, warning, err := mergeRulesFile(r, path, spec.body)
		if err != nil {
			return changed, warnings, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
		changed = changed || c
	}
	return changed, warnings, nil
}

// mergeRulesFile reads the target through the containment root FIRST (to
// preserve the developer's content), so an escaping-symlink target is refused
// here rather than healed the way a write-only path would be — os.Root.ReadFile
// rejects the escape before any splice.
func mergeRulesFile(r *rooted, path, body string) (changed bool, warning string, err error) {
	existing := ""
	if data, e := r.readFile(path); e == nil {
		existing = string(data)
	} else if !os.IsNotExist(e) {
		return false, "", fmt.Errorf("rules: read %s: %w", path, e)
	}
	if !rulesMarkersHealthy(existing) {
		return false, fmt.Sprintf("%s has a malformed ORBEAT-RULES marker (orphan or duplicate) — skipped; repair the block manually", path), nil
	}
	merged, ch := mergeRules(existing, body)
	if ch {
		if e := r.writeAtomic(path, []byte(merged), r.existingPerm(path, 0o644)); e != nil {
			return false, "", fmt.Errorf("rules: %w", e)
		}
	}
	return ch, "", nil
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
// gated independently by stripRulesFile's rulesMarkersHealthy check — the
// returned warnings name every file that was left untouched because of a
// malformed marker. A caller MUST treat a non-empty warnings slice as
// "possibly still carries a block" for ledger-preservation purposes, exactly
// like an I/O failure: an unhealthy AGENTS.md does not block a healthy
// CLAUDE.md from being stripped (and vice versa) — the gate is per-file.
func stripRulesFromProject(proj string, plan *Plan) (stripped int, warnings []string, err error) {
	// S2 containment: strip through a root opened at proj. A gone project dir
	// (openRootedOptional → !ok) means nothing to strip — same as the old
	// per-file os.ReadFile returning not-exist — so callers drop the ledger entry.
	r, ok, err := openRootedOptionalPlanned(proj, plan)
	if err != nil {
		return 0, nil, fmt.Errorf("rules: open project root %s: %w", proj, err)
	}
	if !ok {
		return 0, nil, nil
	}
	defer r.Close()
	for _, file := range rulesManagedFiles {
		path, rerr := resolveContained(proj, file)
		if rerr != nil {
			return stripped, warnings, rerr
		}
		n, warning, serr := stripRulesFile(r, path)
		if serr != nil {
			return stripped, warnings, serr
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
		stripped += n
	}
	return stripped, warnings, nil
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
// If a malformed marker blocked the splice on either managed file, the block
// may still be on disk — proj's ledger entry is preserved rather than
// unconditionally dropped, so a later run retries after manual repair.
func StripProjectRules(claudeDir, proj string) (int, error) {
	m, err := loadManifest(claudeDir)
	if err != nil {
		return 0, err
	}
	stripped, warnings, err := stripRulesFromProject(proj, nil)
	if err != nil {
		return stripped, err
	}
	malformed := len(warnings) > 0
	clean := filepath.Clean(proj)
	kept := make([]string, 0, len(m.Rules))
	for _, p := range m.Rules {
		if filepath.Clean(p) == clean {
			if malformed {
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
		return stripped, err
	}
	return stripped, nil
}
