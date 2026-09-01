package syncclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const manifestName = ".orbeat-sync-manifest.json"

// artifactNameRe mirrors the server's slug rule; it makes path traversal
// (`..`, `/`, absolute paths) impossible when building local file paths.
var artifactNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ReconcileResult summarizes a sync run.
type ReconcileResult struct {
	Added, Updated, Removed int
	// Unchanged counts files whose on-disk bytes already equal the desired
	// content, skipped without a write, so their mtimes are untouched
	// (mirrors Seed/RulesResult.Unchanged). It deliberately does NOT say
	// "managed": a file the ledger did not name and whose bytes already match
	// is adopted into the ledger and counted here (see the write loop's
	// adoption branch), which is what makes "delete the manifest and sync"
	// the recovery doctor advertises.
	Unchanged int
	// Handled counts artifacts of a file-backed type (see fileBackedTypes) that
	// this reconciler processed, regardless of whether the write actually
	// landed (an unmanaged-name collision still counts — the reconciler DID
	// handle it, it just chose not to clobber). Counted per artifact, not per
	// rendered path, so it stays correct even if two artifacts render to the
	// same relative path. This is the authoritative count for callers (e.g.
	// cmd/sync's summary line) — do not recompute it from artifact types
	// outside this package.
	Handled int
	// Skipped names unmanaged files with a colliding name, left untouched.
	// Only ones whose content DIFFERS from the desired content: a byte-identical
	// unmanaged file is adopted instead, since there is nothing there to lose.
	Skipped []string
	// Warnings holds non-fatal notices: an unrecognized artifact type this
	// client skipped, or a Files ledger entry whose shape this client could
	// never have written (validManagedFilePath).
	Warnings []string
	Failures []string // units (or the manifest save) that should have synced but did not (non-fatal I/O)
	// Applied names the artifacts whose content is on disk after this run, at
	// the revision the server served. It is NOT the entitled set, and the gap
	// is the whole point: an unmanaged-name collision leaves the developer's
	// own file in place (see Skipped), and a failed write leaves the previous
	// bytes there (see Failures). Both are served-but-not-applied, and neither
	// is visible from the artifact list the server sent. Unchanged IS applied:
	// the change detection below skips the write only when the bytes already
	// match, and the file being correct is the only thing an applied record
	// claims.
	//
	// Recorded inside the write loop, while the artifact is still in hand. The
	// counters above and manifest.Files carry PATHS, and nothing in this client
	// maps a path back to an artifact id, so an applied set reconstructed
	// afterwards would be a second mapping free to drift from this one.
	//
	// Ordered by ArtifactID, because the write loop ranges over a map.
	//
	// In plan mode (--dry-run) this names what a real run WOULD apply: the
	// writes are recorded rather than performed, so nothing was actually
	// applied and a caller must not report it as such. That filter belongs to
	// the caller, the same division of labour saveManifest documents for the
	// manifest's own recorded write.
	Applied []AppliedArtifact
}

// AppliedArtifact is one artifact a reconciler put (or found already) on disk,
// paired with the revision the server served it as. Both fields come straight
// from Artifact's two unconditional identity fields.
type AppliedArtifact struct {
	ArtifactID string
	Revision   int
}

// appendApplied records id at revision, dropping an artifact the server never
// identified. An empty ID means the server predates the DTO's id field
// (Artifact.ID), so there is no key any deployment record could be stored
// under; an entry naming none is not a fact, and the reporting path must
// not run against such a server at all.
func appendApplied(dst []AppliedArtifact, id string, revision int) []AppliedArtifact {
	if id == "" {
		return dst
	}
	return append(dst, AppliedArtifact{ArtifactID: id, Revision: revision})
}

// desiredFile is one rendered path's content plus the identity of the artifact
// that produced it. The identity rides along because the write loop is the last
// place the artifact is in hand: see ReconcileResult.Applied.
type desiredFile struct {
	content    string
	artifactID string
	revision   int
}

// fileBackedTypes is the single source of truth for which artifact types
// Reconcile renders to disk, and how to compute each one's relative path from
// its name. The map lookup IS the classifier: a type either has an entry here
// (file-backed — rendered below) or it doesn't (owned elsewhere, e.g. "rule"
// is owned by ReconcileRules, or unrecognized by this client version). Adding
// a file-backed type is one edit, here — there is no second table to keep in
// sync (a prior version of this reconciler had a duplicate switch that could
// drift out of sync with this table, silently reintroducing the whole-sync
// abort this reconciler exists to prevent).
var fileBackedTypes = map[string]func(name string) string{
	"skill":    func(n string) string { return "skills/" + n + "/SKILL.md" },
	"subagent": func(n string) string { return "agents/" + n + ".md" },
}

type manifest struct {
	Files []string `json:"files"`
	// Seeds is the ledger of governed memory blocks: subagent name → absolute
	// MEMORY.md paths written (spec §7.5). The in-file markers stay the
	// authoritative record; this is the index of where to look across projects.
	// The ledger is untrusted input — it lives in a user-editable file on
	// disk — so consumers must shape-check each path via validSeedPath before
	// touching it (defense-in-depth against a tampered manifest pointing the
	// strip pass at an arbitrary file).
	Seeds map[string][]string `json:"seeds,omitempty"`
	// Rules is the ledger of project roots that carry an ORBEAT-RULES managed
	// block (AGENTS.md + CLAUDE.md), so a later sync strips exactly the projects
	// no longer entitled/registered. Shape-validated before the strip pass.
	Rules []string `json:"rules,omitempty"`
	// Globals is the ledger of USER-LEVEL instruction files carrying an
	// ORBEAT-RULES block (migration 00025's global scope): absolute paths, so a
	// file whose tool is later uninstalled can still be stripped by path alone.
	// Untrusted like the two ledgers above, shape-checked by
	// validGlobalRulesPath before the strip pass touches anything.
	Globals []string `json:"globals,omitempty"`
}

// Reconcile writes to claudeDir the SUBSET of the given artifacts that this
// client renders to disk (see fileBackedTypes — currently "skill" and
// "subagent"), and removes orbeat-managed files no longer entitled. It NEVER
// modifies or deletes a file it does not manage (tracked via
// claudeDir/.orbeat-sync-manifest.json); a desired path that already exists
// but isn't managed and whose content DIFFERS is skipped (a user's
// hand-authored file wins). One that already holds the exact desired bytes is
// adopted into the ledger instead: nothing on disk changes, and it is the only
// case in which taking ownership can lose nothing.
//
// Artifact types this client doesn't render to disk are intentionally NOT an
// error: a type owned by another reconciler (e.g. "rule", owned by
// ReconcileRules) is skipped silently, and any other unrecognized type is
// skipped with a warning (see ReconcileResult.Warnings). Reconcile must NEVER
// abort the whole sync over a type it doesn't own — a prior version did
// exactly that for "rule" and took down skill/subagent sync with it.
func Reconcile(claudeDir string, artifacts []Artifact, plan *Plan) (ReconcileResult, error) {
	var res ReconcileResult
	m, err := loadManifest(claudeDir)
	if err != nil {
		return res, err
	}
	// Symlink containment (S2): every stat/read/write/remove below runs through a
	// root opened at claudeDir, so an intermediate symlinked directory (e.g. a
	// hostile `skills` symlink escaping ~/.claude) can't land bytes outside it.
	// MkdirAll first so a first-ever sync into a not-yet-created ~/.claude still
	// works — the old writeFileAtomic created it lazily via the first write.
	//
	// In plan mode this MkdirAll is itself a mutation, so it's skipped. A plan
	// against a claudeDir that doesn't exist yet is still fully computable
	// (B1): loadManifest above already returned an empty manifest for the same
	// absent boundary, so oldSet below is empty and every desired artifact is
	// a create with nothing to remove — exactly what a first-ever real sync
	// would do. openRootedFor hands back a *rooted with no underlying os.Root
	// in that case (openRootedPlannedAbsent) whose stat/readFile report every
	// path as absent, rather than the zero-result short-circuit an earlier
	// version of this function returned here.
	if !plan.active() {
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			return res, markFatal(fmt.Errorf("reconcile: create sync root: %w", err))
		}
	}
	root, err := openRootedFor(claudeDir, plan)
	if err != nil {
		return res, markFatal(fmt.Errorf("reconcile: open sync root: %w", err))
	}
	defer root.Close()

	oldSet := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		oldSet[f] = true
	}

	// Keyed by rendered path, so two artifacts rendering to the same path
	// collapse to the last one, which is also the only one whose bytes could
	// end up on disk, and therefore the only one Applied may name. Handled
	// still counts both, per its own doc above.
	desired := make(map[string]desiredFile, len(artifacts))
	for _, a := range artifacts {
		pathFn, ok := fileBackedTypes[a.Type]
		if !ok {
			if a.Type != "rule" {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"unknown artifact type %q (skipped; upgrade orbeat-sync?)", a.Type))
			}
			continue // "rule" is owned by ReconcileRules, not this file reconciler
		}
		res.Handled++
		if !artifactNameRe.MatchString(a.Name) {
			return res, markFatal(fmt.Errorf("reconcile: unsafe artifact name %q", a.Name))
		}
		desired[pathFn(a.Name)] = desiredFile{content: a.Content, artifactID: a.ID, revision: a.Revision}
	}

	managed := make([]string, 0, len(desired))
	for rel, want := range desired {
		full, err := resolveContained(claudeDir, rel)
		if err != nil {
			return res, err // fatal: traversal
		}
		_, statErr := root.stat(full)
		exists := statErr == nil
		if exists {
			// Change detection: a file whose bytes already match is left alone
			// (no write, mtime untouched) so a steady-state sync is a no-op
			// instead of rewriting the whole tree every run. A read error is
			// NOT a failure here: for a ledgered path it falls through and lets
			// the write decide, for an unledgered one it falls through to the
			// collision skip below, which is where an unreadable stranger
			// belongs anyway.
			//
			// The ledger is deliberately NOT consulted for this branch, and
			// that is the whole of the A9 fix. An unledgered file whose bytes
			// already equal the desired content is ADOPTED here: recorded in
			// `managed` so the rebuilt ledger names it, and counted Unchanged.
			// Both doctor remedies that say "delete the manifest entirely and
			// run 'orbeat-sync sync'" were false without it, because every
			// entitled artifact already on disk was classified as an unmanaged
			// collision, skipped, and left out of the rebuilt ledger, freezing
			// it at its current content on that run and on every run after.
			//
			// The narrowness is the safety argument, so do not widen it: bytes
			// EQUAL to the desired content is the one case in which adoption
			// changes nothing on disk and can destroy nothing, because there is
			// no version of the file to lose. A file whose content DIFFERS is
			// still the developer's until they say otherwise, and it keeps the
			// collision skip below.
			//
			// "Destroys nothing" is a claim about THIS run only. Adoption
			// transfers OWNERSHIP: the path enters `managed`, so the rebuilt
			// ledger names it, and a later de-entitlement then authorizes the
			// root.remove in the loop below. The same file this run left
			// untouched is the file a future run deletes. That is what the byte
			// comparison is really buying, and why it is the whole gate: the
			// only file this hands orbeat the right to delete is one whose
			// content orbeat would have written itself.
			// TestReconcileAdoptsAnIdenticalUnledgeredFile puts its load-bearing
			// assertion on exactly that, de-entitling afterwards and requiring
			// the removal, because reporting Unchanged proves nothing about
			// ownership.
			if cur, readErr := root.readFile(full); readErr == nil && string(cur) == want.content {
				res.Unchanged++
				managed = append(managed, rel)
				// Applied: the bytes on disk are the served bytes. A run that
				// wrote nothing at all still deployed everything it was asked
				// to, and a registry that only counted writes would report a
				// steady-state fleet as having nothing installed.
				res.Applied = appendApplied(res.Applied, want.artifactID, want.revision)
				continue
			}
			if !oldSet[rel] {
				res.Skipped = append(res.Skipped, rel) // unmanaged collision, don't clobber
				continue
			}
		}
		// root.writeAtomic creates missing parent dirs and writes temp+rename
		// (contained beneath claudeDir), so a crash mid-write can never leave a
		// torn SKILL.md/agent file behind, and an escaping symlink component
		// fails the write instead of following it out of the root.
		if err := root.writeAtomic(full, []byte(want.content), root.existingPerm(full, 0o644)); err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: reconcile: write: %v", rel, err))
			if oldSet[rel] {
				managed = append(managed, rel) // preserve: update failed, retry next run
			}
			continue
		}
		if oldSet[rel] {
			res.Updated++
		} else {
			res.Added++
		}
		managed = append(managed, rel)
		res.Applied = appendApplied(res.Applied, want.artifactID, want.revision)
	}
	// The loop above ranges over a map, so without this two identical runs
	// would produce different orderings of the same set.
	sort.Slice(res.Applied, func(i, j int) bool { return res.Applied[i].ArtifactID < res.Applied[j].ArtifactID })

	// Remove managed files no longer desired.
	for _, rel := range m.Files {
		if _, keep := desired[rel]; keep {
			continue
		}
		full, err := resolveContained(claudeDir, rel)
		if err != nil {
			return res, err // fatal: traversal
		}
		// A7: the traversal guard above was the ONLY thing standing between an
		// untrusted ledger entry and root.remove, and it passes anything that
		// stays inside the sync root. A manifest holding
		// {"files":["CLAUDE.md","settings.json"]} therefore deleted both:
		// ~/.claude/CLAUDE.md is this client's own global-rules target and
		// ~/.claude/settings.json is Claude Code's configuration. Shape-check the
		// entry the way the three sibling ledgers already shape-check theirs.
		//
		// Ordered AFTER resolveContained on purpose: a traversing entry stays a
		// fatalError that aborts the run at exit 2, which is the contract the
		// fatalError taxonomy states and the state doctor's CheckManifest
		// finding tells the operator about. Refusing it here instead would
		// quietly downgrade a containment escape to a warning.
		//
		// DROPPED, not preserved, and the ledger-preservation rule the rest of
		// this client follows (v1.15.0's cost asymmetry) genuinely does not
		// apply. Preservation buys a retry of a unit this run could not
		// complete; there is no unit here. validManagedFilePath is a pure
		// function of the string FOR A GIVEN BUILD, since it derives its accepted
		// set from fileBackedTypes, so an entry THIS binary refuses it refuses on
		// every run of this binary, and the removal being retried can never
		// happen. Keeping it would reprint this warning forever, which is the
		// stranded entry doctor exists to complain about. Both sibling ledgers
		// that shape-check (validRulesPath, and validGlobalRulesPath with its
		// explicit "a bad line must not become a denial of service" argument)
		// drop too.
		//
		// "Every FUTURE run" would be the stronger claim and it is not available:
		// fileBackedTypes grows, so a downgrade (a newer orbeat-sync that manages
		// a third file-backed type, then this build run against the manifest it
		// left) reaches here holding a perfectly legitimate entry.
		// Dropping it is still right, since this build cannot render, own or
		// remove a file whose type it does not know; the newer client re-adopts
		// it on its next run through the byte-equality branch above, and until
		// then the file sits on disk unmanaged. That is why the warning below
		// does not name a culprit.
		if !validManagedFilePath(rel) {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"ignoring sync ledger file entry %q: this orbeat-sync writes no such path, so it was hand-edited, tampered with, or written by a newer orbeat-sync managing a file type this one does not know; dropped from the ledger, nothing on disk was touched", rel))
			continue
		}
		rmErr := root.remove(full)
		if rmErr != nil && !os.IsNotExist(rmErr) {
			res.Failures = append(res.Failures, fmt.Sprintf("%s: reconcile: remove: %v", rel, rmErr))
			managed = append(managed, rel) // preserve: remove failed, retry next run
			continue
		}
		// Prune an artifact-OWNED subdir (e.g. skills/<name>) only — never a SHARED
		// top-level type dir (agents/), whose removal would delete every subagent's
		// home the moment the last one is de-entitled. rel has >=2 slashes iff the
		// file sits in its own subdirectory (skills/<name>/SKILL.md), not directly
		// under a type dir (agents/<name>.md).
		if strings.Count(rel, "/") >= 2 {
			skillDir := filepath.Dir(full)
			if plan.active() {
				// Plan mode never touches disk — root.remove above only
				// RECORDED the SKILL.md removal, so the file is still
				// physically present here. Simulate what the real rmdir
				// below would decide by reading the directory as it stands
				// right now, excluding that not-yet-deleted file from the
				// count: report the prune only when nothing else would be
				// left behind, matching what a real run's ENOTEMPTY-or-not
				// outcome would be.
				if empty, err := root.dirEmptyExcept(skillDir, filepath.Base(full)); err == nil && empty {
					_ = root.remove(skillDir)
				}
			} else {
				_ = root.remove(skillDir) // now-empty skill dir; ignore "not empty"
			}
		}
		if rmErr == nil {
			res.Removed++ // count only files this run actually removed, not already-missing ones
		}
	}

	m.Files = managed
	if err := saveManifest(claudeDir, m, plan); err != nil {
		res.Failures = append(res.Failures, fmt.Sprintf("manifest: %v", err))
	}
	return res, nil
}

// dirEmptyExcept reports whether the directory at abs — already known to lie
// beneath root's boundary, since it is always derived from a resolveContained
// result — holds no entries other than exceptName. It exists solely to let
// the removal loop's plan-mode branch decide whether a real run's
// skill-directory prune would actually happen, without doing any I/O beyond
// a read. Follows the exact containment pattern contained.go's other
// accessors use (stat/readFile/remove/writeAtomic): resolve via r.rel, then
// operate through the *os.Root — never a bare os call — so a hostile
// symlinked path can't escape containment just because this is a preview.
func (r *rooted) dirEmptyExcept(abs, exceptName string) (bool, error) {
	rel, err := r.rel(abs)
	if err != nil {
		return false, err
	}
	d, err := r.root.Open(rel)
	if err != nil {
		return false, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name() != exceptName {
			return false, nil
		}
	}
	return true, nil
}

// openRootedFor opens a containment root at dir, recording mutations instead
// of performing them when plan is active. Nil plan is the real write path.
//
// When plan is active and dir does not exist, this returns a *rooted with no
// underlying os.Root (openRootedPlannedAbsent) instead of erroring: the
// caller is computing a plan against a boundary a real run would MkdirAll
// before ever reaching here, and every path beneath a nonexistent boundary is
// correctly reported as absent by rooted's nil-root handling (B1).
func openRootedFor(dir string, plan *Plan) (*rooted, error) {
	if !plan.active() {
		return openRooted(dir)
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return openRootedPlannedAbsent(dir, plan), nil
		}
		return nil, err
	}
	return openRootedPlanned(dir, plan)
}

// validManagedFilePath shape-checks one entry of the Files ledger before the
// removal loop acts on it, the way validSeedPath, validRulesPath and
// validGlobalRulesPath shape-check theirs. The manifest is a user-editable
// file on disk, so every entry is untrusted input; this was the only ledger
// whose entries reached an action, root.remove, behind nothing but the
// traversal guard.
//
// The accepted set is DERIVED from fileBackedTypes rather than restated: an
// entry is valid iff some registered type's path function, applied to a name
// this client would accept (artifactNameRe), reproduces the entry byte for
// byte. A hand-written pattern would be exactly the second table
// fileBackedTypes' own doc comment warns about, free to drift the day a third
// file-backed type is added, and drift here means either deleting a file the
// client does own or refusing to clean up one it wrote.
//
// The candidate name is the varying path segment, with and without its
// extension, which covers both shapes the map emits today
// (skills/<slug>/SKILL.md, agents/<slug>.md) and any future one whose only
// variable part is a single segment. A shape whose name spans more than one
// segment would need this loop widened, and TestValidManagedFilePathAccepts-
// EveryFileBackedType fails the moment one is added.
func validManagedFilePath(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		for _, name := range [2]string{seg, strings.TrimSuffix(seg, filepath.Ext(seg))} {
			if !artifactNameRe.MatchString(name) {
				continue
			}
			for _, pathFn := range fileBackedTypes {
				if pathFn(name) == rel {
					return true
				}
			}
		}
	}
	return false
}

// resolveContained joins rel under claudeDir and verifies the result stays inside
// claudeDir — defense-in-depth against traversal from a name or a tampered manifest.
func resolveContained(claudeDir, rel string) (string, error) {
	full := filepath.Join(claudeDir, filepath.FromSlash(rel))
	cleanDir := filepath.Clean(claudeDir)
	if full != cleanDir && !strings.HasPrefix(full, cleanDir+string(os.PathSeparator)) {
		return "", markFatal(fmt.Errorf("reconcile: path %q escapes the sync root", rel))
	}
	return full, nil
}

// loadManifest reads the sync manifest, returning a zero-value manifest (nil
// Files, nil Seeds) if none exists yet. Slice-A manifests (only "files") load
// fine, with Seeds left nil — back-compat is load-bearing here. The read runs
// through a claudeDir root (S2 containment): a missing claudeDir is treated as
// "no manifest yet" (first-ever sync), so this stays callable before the root
// directory exists.
func loadManifest(claudeDir string) (manifest, error) {
	r, ok, err := openRootedOptional(claudeDir)
	if err != nil {
		return manifest{}, markFatal(fmt.Errorf("reconcile: open sync root: %w", err))
	}
	if !ok {
		return manifest{}, nil // claudeDir not created yet
	}
	defer r.Close()
	data, err := r.readFile(filepath.Join(claudeDir, manifestName))
	if err != nil {
		if os.IsNotExist(err) {
			return manifest{}, nil
		}
		return manifest{}, markFatal(fmt.Errorf("reconcile: read manifest: %w", err))
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, markFatal(fmt.Errorf("reconcile: parse manifest: %w", err))
	}
	return m, nil
}

// saveManifest writes the manifest — managed-files ledger plus the seeds/rules
// ledgers, when present — atomically and contained beneath claudeDir (S2). It
// creates claudeDir if absent (some strip paths save before any write pass has
// created it).
//
// In plan mode the write is routed through the planned root like any other
// write, so it is RECORDED, not performed: the Plan is the complete set of
// mutations the run would make, and silently omitting the ledger write would
// misrepresent what the code does (a dry run that rewrites the ledger is not
// a dry run — this is the threading point a plausible implementation misses,
// and the saveManifest red-proof depends on this write being covered). The
// manifest entry is bookkeeping a user-facing --dry-run report shouldn't show;
// filtering it out of that report is the caller's job (cmd/sync), not this
// function's — Plan.Changes stays the honest, complete record.
//
// When planning against a claudeDir that doesn't exist yet, this still
// records the write rather than skipping it (B1): openRootedFor hands back a
// nil-backed *rooted (openRootedPlannedAbsent) rather than erroring, and
// writeAtomic's existing plan branch records through it exactly as it would
// for any other path — the manifest Change joins the rest of the plan's
// creates, keeping Plan.Changes the honest, complete record the paragraph
// above promises. An earlier version of this comment special-cased a missing
// claudeDir as a no-op on the premise that Reconcile always short-circuits
// before ever reaching here in that case; that stopped being true once
// Reconcile itself started computing a real plan against an absent root
// instead of returning the zero result.
func saveManifest(claudeDir string, m manifest, plan *Plan) error {
	sort.Strings(m.Files)
	for _, ps := range m.Seeds {
		sort.Strings(ps)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("reconcile: marshal manifest: %w", err)
	}
	if !plan.active() {
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			return fmt.Errorf("reconcile: create sync root: %w", err)
		}
	}
	r, err := openRootedFor(claudeDir, plan)
	if err != nil {
		return fmt.Errorf("reconcile: open sync root: %w", err)
	}
	defer r.Close()
	return r.writeAtomic(filepath.Join(claudeDir, manifestName), data, 0o644)
}
