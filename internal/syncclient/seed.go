package syncclient

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Governed seed memory (Slice B §7): orbeat-sync owns exactly one
// sentinel-delimited block per subagent at the top of its MEMORY.md; everything
// outside the markers belongs to the agent and is never touched.

// seedHash is the idempotency key: the first 12 hex chars of the SHA-256 of
// body as it is actually written (trailing newlines stripped — see
// renderSeedBlock) — so a body differing from the last-written one only in
// trailing newlines hashes identically and is correctly treated as a no-op.
func seedHash(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(body, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// renderSeedBlock produces the managed block, trailing-newline-terminated.
func renderSeedBlock(name, body string) string {
	return fmt.Sprintf(
		"<!-- ORBEAT-SEED:BEGIN %s sha=%s — managed by orbeat-sync; edit BELOW this block -->\n%s\n<!-- ORBEAT-SEED:END %s -->\n",
		name, seedHash(body), strings.TrimRight(body, "\n"), name)
}

// seedBlockRe matches the whole managed block for name, capturing the hash
// from the BEGIN marker in group 1. The trailing space after BEGIN <name> and
// the " -->" after END <name> prevent prefix collisions (a block for "rev"
// never matches a merge for "rev-two").
func seedBlockRe(name string) *regexp.Regexp {
	q := regexp.QuoteMeta(name)
	return regexp.MustCompile(`(?s)<!-- ORBEAT-SEED:BEGIN ` + q + ` sha=([0-9a-f]{12}) [^\n]*\n.*?<!-- ORBEAT-SEED:END ` + q + ` -->\n?`)
}

// seedBeginRe/seedEndRe are the per-name marker-only patterns backing
// seedMarkersHealthy — narrower than seedBlockRe's full-block match, so an
// orphan BEGIN or a dangling END (neither paired into a full block) is still
// counted on its own.
func seedBeginRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`<!-- ORBEAT-SEED:BEGIN ` + regexp.QuoteMeta(name) + ` `)
}

func seedEndRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`<!-- ORBEAT-SEED:END ` + regexp.QuoteMeta(name) + ` -->`)
}

// seedMarkersHealthy reports whether existing has a clean marker state for
// name — at most one well-formed block and no orphan/duplicate markers.
// Mirrors rulesMarkersHealthy, name-scoped (seed markers embed the artifact
// name, so health is judged per name, not per file): an orphan BEGIN (or a
// hand-copied duplicate) for name can make seedBlockRe(name) span developer
// content — or even a LATER genuine block for the same name — on an in-place
// splice, so a caller must NOT touch such a file; it skips + warns instead.
func seedMarkersHealthy(existing, name string) bool {
	begins := len(seedBeginRe(name).FindAllString(existing, -1))
	ends := len(seedEndRe(name).FindAllString(existing, -1))
	blocks := len(seedBlockRe(name).FindAllString(existing, -1))
	return blocks <= 1 && begins == blocks && ends == blocks
}

// mergeSeed returns content with the governed block for name set to body.
// changed=false means the existing block's BEGIN marker already carries the
// exact hash of body (it is then left exactly where it sits — a no-op stays a
// no-op; the comparison is against the captured hash group, not a substring
// search, so a body that happens to contain a matching " sha=<hash> " token
// cannot be mistaken for the real marker). On change, any existing block is
// removed and the new one is hoisted to the top so it lands within Claude
// Code's first-200-lines auto-load window (spec §7.2). Dropping the old block
// can leave a blank-line run at the removal seam; that run collapses via the
// TrimLeft below when the new block is re-hoisted (intentional — it's what
// keeps the re-hoist tests byte-exact).
func mergeSeed(existing, name, body string) (string, bool) {
	block := renderSeedBlock(name, body)
	if loc := seedBlockRe(name).FindStringSubmatchIndex(existing); loc != nil {
		if existing[loc[2]:loc[3]] == seedHash(body) {
			return existing, false
		}
		// Drop the block plus any leading blank-line gap it leaves behind in
		// the suffix, so a mid-file block doesn't leave the prefix's own
		// trailing blank line doubled up with the block's residual newline.
		existing = existing[:loc[0]] + strings.TrimLeft(existing[loc[1]:], "\n")
	}
	rest := strings.TrimLeft(existing, "\n")
	if rest == "" {
		return block, true
	}
	return block + "\n" + rest, true
}

// stripSeed removes the governed block for name (de-entitlement); the agent's
// own notes are preserved and the file is never deleted (spec §7.5).
func stripSeed(existing, name string) (string, bool) {
	loc := seedBlockRe(name).FindStringIndex(existing)
	if loc == nil {
		return existing, false
	}
	out := existing[:loc[0]] + existing[loc[1]:]
	if loc[0] == 0 {
		out = strings.TrimLeft(out, "\n")
	}
	return out, true
}

var seedNameRe = regexp.MustCompile(`<!-- ORBEAT-SEED:BEGIN ([a-z0-9][a-z0-9-]*) `)

// seedNamesIn lists the subagent names of every managed block in content.
func seedNamesIn(content string) []string {
	var names []string
	for _, m := range seedNameRe.FindAllStringSubmatch(content, -1) {
		names = append(names, m[1])
	}
	return names
}

// SeedResult summarizes the seed pass of a sync run.
type SeedResult struct {
	Written, Unchanged, Stripped int
	Warnings                     []string
	Failures                     []string // targets (or the manifest save) that should have synced but did not (non-fatal I/O)
}

// seedTarget is one MEMORY.md a governed seed must land in. boundary is the
// containment root the path lives under (claudeDir for user scope, the project
// root for project scope) — every read/write for this target routes through a
// root opened at boundary (S2 symlink containment).
type seedTarget struct {
	name, boundary, path, body string
}

// ReconcileSeeds is the Slice-B seed pass (spec §7): for every entitled
// user/project-scope subagent with a seed, merge its ORBEAT-SEED block into
// the target MEMORY.md(s); then strip every previously-written block that is
// no longer desired (de-entitlement, cleared seed, scope change). Files are
// never deleted; writes are atomic; the manifest's seeds ledger records what
// was written where.
//
// A nil error does NOT mean every target synced: a per-target read/write
// failure and a per-candidate strip failure are both non-fatal — they are
// recorded in SeedResult.Failures and the run continues, isolating one broken
// target/project from starving the rest. A non-nil error means a whole-sync
// abort (an unsafe artifact name, a corrupt manifest, or a path escaping the
// sync root — see fatalError/markFatal/isFatal). Any path that failed this
// run keeps its PRIOR ledger entry (if it had one) so a later run retries it
// instead of orphaning the block — this matters because a project can be
// de-registered before the next successful sync, at which point the fs-scan
// net below no longer walks it and the ledger becomes the only way back.
func ReconcileSeeds(claudeDir string, projects []string, artifacts []Artifact, plan *Plan) (SeedResult, error) {
	var res SeedResult
	m, err := loadManifest(claudeDir)
	if err != nil {
		return res, err
	}

	// S2 containment: MkdirAll claudeDir so user-scope seeds and the manifest
	// have a root to open; project roots are opened lazily per boundary via the
	// cache. Every read/write below runs through one of these roots, so an
	// intermediate symlinked directory (e.g. a hostile `.claude/agent-memory`
	// escaping a cloned repo) fails the operation instead of escaping.
	//
	// This MkdirAll bypasses rooted entirely (there's no root open yet to route
	// it through), so plan mode has no way to record it — it is skipped outright
	// rather than silently performed: a dry run that creates the sync root is
	// still a write.
	if !plan.active() {
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			return res, markFatal(fmt.Errorf("seed: create sync root: %w", err))
		}
	}
	roots := newRootCachePlanned(plan)
	if plan.active() {
		if _, err := os.Stat(claudeDir); err != nil {
			if !os.IsNotExist(err) {
				return res, markFatal(fmt.Errorf("seed: stat sync root: %w", err))
			}
			// Planning against a sync root that doesn't exist yet (B1): a real
			// run's MkdirAll above guarantees claudeDir exists for every
			// roots.get(claudeDir) call that follows, so pre-seed the cache
			// with a nil-backed root for THIS boundary (openRootedPlannedAbsent)
			// to mirror that guarantee — every user-scope target then plans as
			// a create instead of the cache reporting "vanished" and the write
			// pass recording a bogus failure. Project boundaries are
			// unaffected: a project directory is never auto-created by
			// orbeat-sync, so a genuinely missing one still misses the cache
			// below and fails exactly as before.
			roots.seed(claudeDir, openRootedPlannedAbsent(claudeDir, plan))
		}
	}
	defer roots.closeAll()

	// Check each registered project's liveness once — up front, not once per
	// artifact — so a dead project yields exactly one warning no matter how
	// many project-scope artifacts are entitled.
	live := make([]string, 0, len(projects))
	for _, proj := range projects {
		if st, err := os.Stat(proj); err != nil || !st.IsDir() {
			res.Warnings = append(res.Warnings, fmt.Sprintf("registered project %s missing — skipped", proj))
			continue
		}
		live = append(live, proj)
	}

	var targets []seedTarget
	for _, a := range artifacts {
		if a.Type != "subagent" || a.MemorySeed == "" {
			continue
		}
		if !artifactNameRe.MatchString(a.Name) {
			return res, markFatal(fmt.Errorf("seed: unsafe artifact name %q", a.Name))
		}
		switch a.MemoryScope {
		case "user":
			full, err := resolveContained(claudeDir, "agent-memory/"+a.Name+"/MEMORY.md")
			if err != nil {
				return res, err
			}
			targets = append(targets, seedTarget{a.Name, filepath.Clean(claudeDir), full, a.MemorySeed})
		case "project":
			for _, proj := range live {
				full, err := resolveContained(proj, ".claude/agent-memory/"+a.Name+"/MEMORY.md")
				if err != nil {
					return res, err
				}
				targets = append(targets, seedTarget{a.Name, filepath.Clean(proj), full, a.MemorySeed})
			}
		case "local":
			// A known scope, but local memory is Claude-Code-managed and never seeded
			// by orbeat-sync (api/sync.go doesn't even deliver a seed for it). Skip
			// silently — distinct from the unrecognized-scope warning below.
		default:
			// Forward-compat: a scope this client version doesn't know (a future
			// server value) must be surfaced, not silently dropped — mirroring
			// Reconcile's unknown-artifact-type warning.
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"subagent %q has unrecognized memory scope %q (skipped; upgrade orbeat-sync?)", a.Name, a.MemoryScope))
		}
	}

	desired := make(map[string]bool, len(targets)) // key: name + "\x00" + path
	newSeeds := map[string][]string{}
	for _, tg := range targets {
		desired[tg.name+"\x00"+tg.path] = true
	}

	// failedPaths tracks every target this run failed to read/write/strip, so a
	// prior ledger entry for it can be preserved (see the re-add loop below)
	// instead of dropped — and so the strip pass doesn't re-touch (and
	// double-report) a path the write pass already gave up on.
	failedPaths := map[string]bool{}

	// Write/update pass.
	for _, tg := range targets {
		r, ok, err := roots.get(tg.boundary)
		if err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: seed: open root: %v", tg.path, err))
			failedPaths[tg.path] = true
			continue
		}
		if !ok {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: seed: containment root %s vanished", tg.path, tg.boundary))
			failedPaths[tg.path] = true
			continue
		}
		existing := ""
		if data, err := r.readFile(tg.path); err == nil {
			existing = string(data)
		} else if !os.IsNotExist(err) {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: seed: read: %v", tg.path, err))
			failedPaths[tg.path] = true
			continue
		}
		if !seedMarkersHealthy(existing, tg.name) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("malformed ORBEAT-SEED marker (orphan or duplicate) for %q in %s — skipped; repair the block manually", tg.name, tg.path))
			// Still desired: the target stays live/entitled even though this run
			// couldn't splice it, so its ledger entry must persist for a retry
			// after manual repair. This mirrors rules.go's mergeRulesFile skip,
			// which likewise returns no error and lets the live project's normal
			// ledger bookkeeping carry the entry forward.
			newSeeds[tg.name] = append(newSeeds[tg.name], tg.path)
			continue
		}
		merged, changed := mergeSeed(existing, tg.name, tg.body)
		if changed {
			if err := r.writeAtomic(tg.path, []byte(merged), r.existingPerm(tg.path, 0o644)); err != nil {
				res.Failures = append(res.Failures, fmt.Sprintf("%s: seed: write: %v", tg.path, err))
				failedPaths[tg.path] = true
				continue
			}
			res.Written++
		} else {
			res.Unchanged++
		}
		newSeeds[tg.name] = append(newSeeds[tg.name], tg.path)
	}

	// Strip pass: candidates = ledger paths (covers unregistered projects) ∪
	// a scan of the currently managed roots (covers hand-copied files). Each
	// candidate carries the containment boundary its strip must route through
	// (claudeDir for user scope, the project root otherwise — see seedBoundary).
	candidates := map[string]string{} // path -> boundary
	for _, paths := range m.Seeds {
		for _, p := range paths {
			if validSeedPath(p) {
				candidates[p] = seedBoundary(claudeDir, p)
			}
		}
	}
	for _, p := range scanSeedFiles(filepath.Join(claudeDir, "agent-memory")) {
		candidates[p] = filepath.Clean(claudeDir)
	}
	for _, proj := range projects {
		for _, p := range scanSeedFiles(filepath.Join(proj, ".claude", "agent-memory")) {
			candidates[p] = filepath.Clean(proj)
		}
	}
	cleanClaudeDir := filepath.Clean(claudeDir)
	warnedUnreachable := map[string]bool{}
	for p, boundary := range candidates {
		if failedPaths[p] {
			continue // already recorded in the write pass; don't re-read or double-report
		}
		r, ok, err := roots.get(boundary)
		if err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: seed: open root: %v", p, err))
			failedPaths[p] = true
			continue
		}
		if !ok {
			// S5: an unreachable PROJECT root (an unmounted volume — ENOENT here is
			// indistinguishable from a genuine delete) may still carry the block.
			// Preserve the ledger entry (generalizing rules.go's notLive rule to
			// UNREGISTERED entries too) so a later run strips it once the root is
			// reachable, instead of dropping it now and orphaning the block forever.
			//
			// boundary == cleanClaudeDir never reaches here: a user-scope
			// candidate can only come from the ledger (m.Seeds, loaded above)
			// or the fs-scan just above, and a claudeDir this run cannot see
			// contributes to neither — loadManifest already returned an empty
			// manifest for it, and scanSeedFiles returns nil for a directory it
			// cannot read. That holds whether claudeDir physically exists right
			// now or not, in both real and plan mode. (An earlier version of
			// this comment instead claimed "claudeDir is MkdirAll'd up front,
			// so it never lands here" — false in plan mode, which explicitly
			// skips that MkdirAll; see above.)
			if boundary != cleanClaudeDir {
				failedPaths[p] = true
				if !warnedUnreachable[boundary] {
					warnedUnreachable[boundary] = true
					res.Warnings = append(res.Warnings, fmt.Sprintf("project %s is unreachable — its seed block(s) (if any) remain; they will be stripped when the path is reachable and a sync runs", boundary))
				}
			}
			continue // boundary directory gone: nothing to strip here
		}
		n, warnings, err := stripUndesired(r, p, desired)
		if err != nil {
			// No fatal source reaches stripUndesired today; the check keeps a
			// future one routed correctly.
			if isFatal(err) {
				return res, err
			}
			res.Failures = append(res.Failures, fmt.Sprintf("%s: seed: strip: %v", p, err))
			failedPaths[p] = true
			continue
		}
		if len(warnings) > 0 {
			res.Warnings = append(res.Warnings, warnings...)
			// Preserve: a malformed marker blocked the splice, so the block may
			// still be on disk — a later run after manual repair must retry it.
			failedPaths[p] = true
		}
		res.Stripped += n
	}

	// Preserve prior ledger entries for paths this run failed to touch, so a
	// later run retries instead of orphaning the block. The fs-scan net below
	// only walks REGISTERED projects, so it cannot recover an entry for a
	// project that is de-registered before the next successful sync — dropping
	// the entry there would strand the block permanently.
	for name, paths := range m.Seeds {
		for _, p := range paths {
			if !failedPaths[p] {
				continue
			}
			already := false
			for _, q := range newSeeds[name] {
				if q == p {
					already = true
					break
				}
			}
			if !already {
				newSeeds[name] = append(newSeeds[name], p)
			}
		}
	}

	// Dedupe each name's paths before saving (mirrors rules.go's newLedger dedupe):
	// a duplicated `projects` input, or the preservation re-add above, could append
	// the same path more than once.
	for name, paths := range newSeeds {
		seen := make(map[string]bool, len(paths))
		deduped := make([]string, 0, len(paths))
		for _, p := range paths {
			if seen[p] {
				continue
			}
			seen[p] = true
			deduped = append(deduped, p)
		}
		newSeeds[name] = deduped
	}

	m.Seeds = newSeeds
	if len(m.Seeds) == 0 {
		m.Seeds = nil // keep the manifest minimal (omitempty)
	}
	if err := saveManifest(claudeDir, m, plan); err != nil {
		res.Failures = append(res.Failures, fmt.Sprintf("manifest: %v", err))
	}
	return res, nil
}

// validSeedPath accepts only absolute paths shaped like a per-subagent memory
// file (…/agent-memory/<slug>/MEMORY.md) — defense-in-depth against a
// tampered manifest pointing the strip pass at arbitrary files.
func validSeedPath(p string) bool {
	if !filepath.IsAbs(p) || filepath.Base(p) != "MEMORY.md" {
		return false
	}
	dir := filepath.Dir(p)
	if !artifactNameRe.MatchString(filepath.Base(dir)) {
		return false
	}
	return filepath.Base(filepath.Dir(dir)) == "agent-memory"
}

// scanSeedFiles lists <root>/<name>/MEMORY.md files under an agent-memory
// root, for every entry whose name matches artifactNameRe (the same slug
// shape validSeedPath requires) and that actually has a regular-file
// MEMORY.md inside it. A missing or unreadable root returns nil — the same
// tolerant behavior as a no-match glob.
func scanSeedFiles(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !artifactNameRe.MatchString(e.Name()) {
			continue
		}
		mem := filepath.Join(root, e.Name(), "MEMORY.md")
		if st, err := os.Stat(mem); err == nil && st.Mode().IsRegular() {
			out = append(out, mem)
		}
	}
	return out
}

// StripProjectSeeds removes every ORBEAT-SEED block under proj's
// .claude/agent-memory tree and drops those paths from the ledger — the
// `project remove` semantics (spec §7.4): stop managing means leave no
// governed content behind. Agent notes and files always survive.
// proj must be the cleaned absolute path (as returned by AddProject/RemoveProject);
// a relative path would silently miss the ledger's absolute entries.
func StripProjectSeeds(claudeDir, proj string) (int, error) {
	m, err := loadManifest(claudeDir)
	if err != nil {
		return 0, err
	}
	prefix := filepath.Clean(proj) + string(os.PathSeparator)

	candidates := map[string]bool{}
	for _, p := range scanSeedFiles(filepath.Join(proj, ".claude", "agent-memory")) {
		candidates[p] = true
	}
	for _, paths := range m.Seeds {
		for _, p := range paths {
			if strings.HasPrefix(p, prefix) && validSeedPath(p) {
				candidates[p] = true
			}
		}
	}

	stripped := 0
	nothingDesired := map[string]bool{}
	// malformed tracks paths a malformed marker blocked from stripping — the
	// ledger entry for such a path must survive the removal below (the block
	// may still be on disk; a later run after manual repair must retry it),
	// unlike a healthy path's entry, which "project remove" always forgets.
	malformed := map[string]bool{}
	// S2 containment: strip through a root opened at proj. A gone project dir
	// (openRootedOptional → !ok) means nothing to strip — the ledger cleanup
	// below then drops every proj-prefixed entry, exactly as before.
	r, ok, err := openRootedOptional(proj)
	if err != nil {
		return 0, fmt.Errorf("seed: open project root %s: %w", proj, err)
	}
	if ok {
		defer r.Close()
		for p := range candidates {
			n, warnings, err := stripUndesired(r, p, nothingDesired)
			if err != nil {
				return stripped, err
			}
			if len(warnings) > 0 {
				malformed[p] = true
			}
			stripped += n
		}
	}

	for name, paths := range m.Seeds {
		kept := make([]string, 0, len(paths))
		for _, p := range paths {
			if strings.HasPrefix(p, prefix) {
				if malformed[p] {
					kept = append(kept, p)
				}
				continue
			}
			kept = append(kept, p)
		}
		if len(kept) == 0 {
			delete(m.Seeds, name)
		} else {
			m.Seeds[name] = kept
		}
	}

	if len(m.Seeds) == 0 {
		m.Seeds = nil
	}
	if err := saveManifest(claudeDir, m, nil); err != nil {
		return stripped, err
	}
	return stripped, nil
}

// stripUndesired removes every ORBEAT-SEED block at path whose (name, path) is
// not desired. Missing files are fine (ledger entry for a deleted project). A
// name whose markers are unhealthy (orphan/duplicate BEGIN or END — see
// seedMarkersHealthy) is left untouched and reported via the returned
// warnings instead of stripped: splicing it risks spanning into surrounding
// developer content or even a later genuine block for the same name. The
// caller must treat a path with any returned warning as failed for
// ledger-preservation purposes — the block may still be on disk.
func stripUndesired(r *rooted, path string, desired map[string]bool) (stripped int, warnings []string, err error) {
	data, err := r.readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, nil
		}
		return 0, nil, fmt.Errorf("seed: read %s: %w", path, err)
	}
	content := string(data)
	seen := map[string]bool{} // dedupes a name that seedNamesIn reports more than once (e.g. an orphan BEGIN plus a genuine one for the same name)
	for _, name := range seedNamesIn(content) {
		if seen[name] {
			continue
		}
		seen[name] = true
		if desired[name+"\x00"+path] {
			continue
		}
		if !seedMarkersHealthy(content, name) {
			warnings = append(warnings, fmt.Sprintf("malformed ORBEAT-SEED marker (orphan or duplicate) for %q in %s — skipped; repair the block manually", name, path))
			continue
		}
		if out, changed := stripSeed(content, name); changed {
			content = out
			stripped++
		}
	}
	if stripped > 0 {
		if err := r.writeAtomic(path, []byte(content), r.existingPerm(path, 0o644)); err != nil {
			return 0, warnings, fmt.Errorf("seed: %w", err)
		}
	}
	return stripped, warnings, nil
}

// seedBoundary returns the containment root a seed MEMORY.md path lives under.
// User-scope seeds sit at <claudeDir>/agent-memory/<name>/MEMORY.md (under
// claudeDir); project-scope at <proj>/.claude/agent-memory/<name>/MEMORY.md, so
// the project root is four directories up (…/<name> → …/agent-memory → …/.claude
// → <proj>). Called only for validSeedPath-shaped inputs. This assumes a real
// project layout; a tampered manifest entry with a different shape yields a
// mismatched boundary, which os.Root then rejects on the rel() check — the
// boundary can only be narrower than the path, never wider, so a wrong guess
// fails the operation rather than widening containment.
func seedBoundary(claudeDir, p string) string {
	cd := filepath.Clean(claudeDir)
	if p == cd || strings.HasPrefix(p, cd+string(os.PathSeparator)) {
		return cd
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(p))))
}
