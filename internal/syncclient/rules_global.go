package syncclient

import (
	"fmt"
	"os"
	"path/filepath"
)

// Global-scope rules (migration 00025).
//
// A project rule lands in each registered project's AGENTS.md. A GLOBAL rule
// lands in the user-level instruction files every project inherits, which is
// where an instruction about the DEVELOPER belongs rather than one about a
// repository: "always ask before force-pushing" is not a property of a repo.
//
// Two targets, both verified against the tools' own documentation rather than
// assumed, because a file an agent never reads is a governance control that
// silently does nothing:
//
//   - <claudeDir>/CLAUDE.md. Claude Code's global-scope files live in ~/.claude
//     and its CLAUDE.md there is user-scope memory, read at the start of every
//     session (code.claude.com/docs/en/memory, /claude-directory). claudeDir is
//     already this client's root for user-scope writes, and CLAUDE_CONFIG_DIR
//     moves both together, so nothing here has to know about that variable.
//   - <codexHome>/AGENTS.md, only when that directory ALREADY EXISTS. Codex
//     reads global instructions from AGENTS.md in its home directory, usually
//     ~/.codex (learn.chatgpt.com/docs/agent-configuration/agents-md).
//
// The Codex directory is never created. Creating a tool's home to leave a file
// in it would be this client inventing an installation that does not exist, and
// `orbeat-sync connect` already treats a missing tool as "not installed" rather
// than as something to provision.
//
// AGENTS.override.md, which Codex prefers over AGENTS.md when present, is
// deliberately NOT written: it is the file a developer uses to override
// org-wide instructions, so writing org-wide instructions into it would take
// away the escape hatch it exists to be.
type globalTarget struct {
	Dir  string
	File string
}

// globalRuleTargets lists the user-level files to manage on this machine.
func globalRuleTargets(claudeDir string) []globalTarget {
	out := []globalTarget{{Dir: claudeDir, File: "CLAUDE.md"}}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	codex := filepath.Join(home, ".codex")
	if st, err := os.Stat(codex); err == nil && st.IsDir() {
		out = append(out, globalTarget{Dir: codex, File: "AGENTS.md"})
	}
	return out
}

// Path is the absolute file this target manages; it is what the manifest
// ledgers, so a target whose tool is later uninstalled can still be stripped by
// path alone.
func (g globalTarget) Path() string { return filepath.Join(g.Dir, g.File) }

// validGlobalRulesPath shape-checks a ledger entry before the strip pass
// touches it. The manifest is a user-editable file, so an entry is untrusted
// input in exactly the way the Rules ledger is: absolute, and one of the two
// file names this client ever writes at global scope. Without the name check a
// tampered manifest could point the strip pass at any file in any directory.
//
// SHAPE ALONE IS NOT ENOUGH (B23): a well-formed absolute path named
// "CLAUDE.md" sitting in a directory this client never manages passes this
// check just as easily as the real one does. isTrustedGlobalRulesPath is the
// second, load-bearing gate the strip pass also applies — see its own doc
// comment for why.
func validGlobalRulesPath(p string) bool {
	if !filepath.IsAbs(p) || p != filepath.Clean(p) {
		return false
	}
	switch filepath.Base(p) {
	case "CLAUDE.md", "AGENTS.md":
		return true
	}
	return false
}

// allGlobalRuleTargets is the full, EXISTENCE-INDEPENDENT set of user-level
// files this client may ever manage an ORBEAT-RULES block in: claudeDir's
// CLAUDE.md always, and the Codex home's AGENTS.md whether or not ~/.codex
// currently exists. Contrast with globalRuleTargets (the WRITE pass), which
// omits Codex when it is not installed, because this client never creates a
// tool's home directory (see its own doc comment) — but the STRIP pass has to
// trust a ledger entry for a tool that has since been uninstalled (see
// stripGlobalRules's doc comment: a vanished directory there is not an
// error), so the set used to VALIDATE a ledger entry cannot be gated on
// current existence, only on which paths this client could ever have
// written.
func allGlobalRuleTargets(claudeDir string) []globalTarget {
	out := []globalTarget{{Dir: filepath.Clean(claudeDir), File: "CLAUDE.md"}}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, globalTarget{Dir: filepath.Join(home, ".codex"), File: "AGENTS.md"})
	}
	return out
}

// isTrustedGlobalRulesPath reports whether path is exactly one of
// allGlobalRuleTargets(claudeDir)'s paths — the trusted set the strip pass
// must derive its containment root from, never from path itself (B23, the
// same construction trustedSeedBoundary closed for seeds: seed.go).
// validGlobalRulesPath alone only checks SHAPE (absolute, clean, named
// CLAUDE.md or AGENTS.md), which a tampered or stale manifest entry can
// satisfy for ANY directory — stripGlobalRules would then open a containment
// root at that untrusted directory (filepath.Dir(path)) and strip a block
// from a file this client never wrote.
func isTrustedGlobalRulesPath(claudeDir, path string) bool {
	for _, g := range allGlobalRuleTargets(claudeDir) {
		if g.Path() == path {
			return true
		}
	}
	return false
}

// writeGlobalRules merges body into one user-level file, through a root opened
// at its directory so a symlinked CLAUDE.md cannot redirect the write outside
// it. Returns whether the file changed and the merge notes (a malformed-marker
// skip, an A8 restoration), with exactly the semantics writeRulesToProject has
// for a project file.
func writeGlobalRules(g globalTarget, body string, plan *Plan) (bool, rulesMergeNotes, error) {
	var notes rulesMergeNotes
	r, ok, err := openRootedOptionalPlanned(g.Dir, plan)
	if err != nil {
		return false, notes, fmt.Errorf("rules: open global root %s: %w", g.Dir, err)
	}
	if !ok {
		return false, notes, fmt.Errorf("rules: global root %s vanished", g.Dir)
	}
	defer r.Close()

	path, err := resolveContained(g.Dir, g.File)
	if err != nil {
		return false, notes, err
	}
	return mergeRulesFile(r, path, body)
}

// stripGlobalRules removes the managed block from one previously-written
// user-level file. A file whose directory has since disappeared is not an
// error: the tool was uninstalled, the block went with it, and reporting a
// failure would keep a ledger entry alive for a file that cannot exist.
func stripGlobalRules(path string, plan *Plan) (int, string, error) {
	dir := filepath.Dir(path)
	r, ok, err := openRootedOptionalPlanned(dir, plan)
	if err != nil {
		return 0, "", fmt.Errorf("rules: open global root %s: %w", dir, err)
	}
	if !ok {
		return 0, "", nil
	}
	defer r.Close()
	return stripRulesFile(r, path)
}

// reconcileGlobalRules brings the user-level files to the desired state and
// updates the Globals ledger, mirroring the project pass in rules.go: write
// where rules apply, strip where a previous run wrote and this one does not,
// and preserve a ledger entry for anything it could not complete so a later run
// retries instead of orphaning a block (the v1.15.0 contract).
//
// It takes the manifest by pointer because the caller saves it once, after both
// passes: two saves would leave a window where the project ledger is current
// and the global one is not, and a crash in that window is exactly the orphan
// the ledger exists to prevent.
func reconcileGlobalRules(claudeDir string, globals []Artifact, m *manifest, res *RulesResult, plan *Plan) error {
	desired := map[string]bool{}
	var newLedger []string

	if len(globals) > 0 {
		body := renderRulesBody(globals)
		landed := false
		for _, g := range globalRuleTargets(claudeDir) {
			path := g.Path()
			changed, notes, err := writeGlobalRules(g, body, plan)
			if err != nil {
				if isFatal(err) {
					return err
				}
				res.Failures = append(res.Failures, fmt.Sprintf("%s: global rules: write: %v", path, err))
				// Preserve unconditionally, for the same reason the project
				// pass does: the write may have landed before the failure, and
				// a missed entry orphans the block forever while a spurious one
				// self-corrects on the next strip.
				newLedger = append(newLedger, path)
				continue
			}
			res.Warnings = append(res.Warnings, notes.all()...)
			// Only a malformed-marker skip can withhold the applied claim, for
			// the reason writeRulesToProject's caller spells out: an A8
			// restoration put the desired bytes on disk, so the file IS in the
			// desired state.
			if len(notes.skips) == 0 {
				landed = true
			}
			if changed {
				res.Written++
			} else {
				res.Unchanged++
			}
			desired[path] = true
			newLedger = append(newLedger, path)
		}
		if landed {
			for _, r := range globals {
				res.Applied = appendApplied(res.Applied, r.ID, r.Revision)
			}
		}
	}

	for _, path := range m.Globals {
		if desired[path] {
			continue
		}
		if !validGlobalRulesPath(path) {
			// A tampered or hand-edited entry: drop it rather than act on it.
			// Not fatal, because refusing to sync over a bad ledger line would
			// hand anyone who can write the manifest a denial of service.
			res.Warnings = append(res.Warnings, fmt.Sprintf("ignoring malformed global rules ledger entry %q", path))
			continue
		}
		if !isTrustedGlobalRulesPath(claudeDir, path) {
			// Well-formed (right basename) but not one of the directories
			// this client actually manages at global scope (B23) — PRESERVED
			// rather than dropped, for the same reason the seeds/rules
			// untrusted-boundary branches preserve theirs: this run did not
			// verify the block is gone.
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"sync ledger entry %s is not one of the user-level files orbeat-sync manages; skipped — orbeat-sync will not touch it", path))
			newLedger = append(newLedger, path)
			continue
		}
		n, warning, err := stripGlobalRules(path, plan)
		if err != nil {
			if isFatal(err) {
				return err
			}
			res.Failures = append(res.Failures, fmt.Sprintf("%s: global rules: strip: %v", path, err))
			newLedger = append(newLedger, path)
			continue
		}
		if warning != "" {
			res.Warnings = append(res.Warnings, warning)
			newLedger = append(newLedger, path) // malformed markers: leave it, keep watching it
			continue
		}
		res.Stripped += n
	}

	m.Globals = newLedger
	if len(m.Globals) == 0 {
		m.Globals = nil
	}
	return nil
}
