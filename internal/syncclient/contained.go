package syncclient

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Symlink containment (audit S2). Every orbeat-sync writer routes its FS
// operations through an os.Root opened at a trusted boundary — claudeDir for the
// file reconciler and user-scope seeds, each registered project root for
// project-scope seeds and rules. os.Root is Go 1.25+'s kernel-enforced
// containment (openat2/RESOLVE_BENEATH on Linux, an equivalent per-component
// resolve elsewhere): any file name whose resolution — through an intermediate
// symlinked directory OR a symlink at the leaf — would land outside the boundary
// makes the operation fail, immune to the TOCTOU window an
// EvalSymlinks-then-write design leaves open. os.Root additionally rejects ALL
// absolute symlinks, dangling or not.
//
// The verified threat this closes is an INTERMEDIATE directory symlink: a cloned
// repo registered as a project whose `.claude/agent-memory` (seeds) is a symlink
// escaping the repo, or a `skills`/`agents` component under ~/.claude replaced by
// an escaping symlink. writeFileAtomic's temp+rename already neutralizes a
// symlink placed AT the final target path (rename replaces the link in place, it
// does not follow it) — so the audit's "dangling symlink at target" escape does
// not reproduce against the current writer; os.Root still rejects that case as
// defense-in-depth should the writer ever change.
//
// A containment rejection surfaces as an ordinary (NON-fatal) error, recorded in
// the caller's Failures with the usual v1.15.0 ledger preservation — it is not a
// fatalError, so it never re-arms the abort cascade.

// rooted pairs an *os.Root with the cleaned absolute path of its boundary, so
// callers keep working in absolute paths (the manifest ledger and fs-scan
// results are all absolute) while every FS operation is contained beneath the
// boundary.
type rooted struct {
	root *os.Root
	dir  string // filepath.Clean(boundary)

	// plan, when non-nil, turns every mutating method into a recorder: the
	// intent is captured and NO I/O happens. Nil is the real write path, so
	// every existing construction site keeps its behaviour unchanged.
	plan *Plan
}

// openRooted opens a containment root at dir, which must already exist.
func openRooted(dir string) (*rooted, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &rooted{root: r, dir: filepath.Clean(dir)}, nil
}

// openRootedPlanned is openRooted with mutations recorded instead of performed.
func openRootedPlanned(dir string, p *Plan) (*rooted, error) {
	r, err := openRooted(dir)
	if err != nil {
		return nil, err
	}
	r.plan = p
	return r, nil
}

// openRootedOptionalPlanned is openRootedOptional with mutations recorded.
func openRootedOptionalPlanned(dir string, p *Plan) (*rooted, bool, error) {
	r, ok, err := openRootedOptional(dir)
	if err != nil || !ok {
		return r, ok, err
	}
	r.plan = p
	return r, true, nil
}

// openRootedPlannedAbsent returns a *rooted bound to dir with NO underlying
// os.Root, for planning against a boundary that does not exist yet (B1): a
// real (non-plan) run would MkdirAll dir before ever opening a root on it, so
// a plan computed against the same not-yet-created dir is fully derivable —
// every path beneath it is absent, so every desired write plans as a create
// and there is nothing to remove. Every rooted method that would otherwise
// dereference r.root (stat, readFile) instead reports not-exist; writeAtomic
// and remove already route through those in plan mode and need no change.
// Callers must only construct this when plan is active — Close is nil-safe
// on the resulting *rooted's absent root, and rel's lexical containment
// check does not depend on r.root, so escape refusal is unaffected by its
// absence (see TestRootedNilRootStillRefusesEscape).
func openRootedPlannedAbsent(dir string, plan *Plan) *rooted {
	return &rooted{dir: filepath.Clean(dir), plan: plan}
}

// openRootedOptional opens a containment root at dir. A missing dir yields
// (nil, false, nil): there is nothing beneath it to contain, so a caller treats
// it as "no files here" (e.g. a de-registered project whose directory is gone —
// the same tolerant behavior os.ReadDir/os.ReadFile gave before). Any other
// error (a non-directory, a permission failure) is returned.
func openRootedOptional(dir string) (*rooted, bool, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &rooted{root: r, dir: filepath.Clean(dir)}, true, nil
}

// Close releases the root's file descriptor. Safe on a nil receiver.
func (r *rooted) Close() {
	if r != nil && r.root != nil {
		_ = r.root.Close()
	}
}

// rel converts an absolute path under the boundary to a path usable with os.Root
// methods. It rejects a path that lexically escapes the boundary — the cheap
// first line (os.Root then enforces symlink containment on the resolved path).
func (r *rooted) rel(abs string) (string, error) {
	rel, err := filepath.Rel(r.dir, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the sync root %q", abs, r.dir)
	}
	return rel, nil
}

// stat is a containment-checked os.Stat (follows symlinks, but not out of root).
// r.root is nil only for a plan-mode root opened via openRootedPlannedAbsent
// against a boundary that does not exist yet — nothing beneath an absent
// boundary can exist either, so that case reports not-exist without ever
// dereferencing r.root.
func (r *rooted) stat(abs string) (os.FileInfo, error) {
	rel, err := r.rel(abs)
	if err != nil {
		return nil, err
	}
	if r.root == nil {
		return nil, &fs.PathError{Op: "stat", Path: rel, Err: fs.ErrNotExist}
	}
	return r.root.Stat(rel)
}

// readFile is a containment-checked os.ReadFile. See stat for the nil-root case.
func (r *rooted) readFile(abs string) ([]byte, error) {
	rel, err := r.rel(abs)
	if err != nil {
		return nil, err
	}
	if r.root == nil {
		return nil, &fs.PathError{Op: "open", Path: rel, Err: fs.ErrNotExist}
	}
	return r.root.ReadFile(rel)
}

// remove is a containment-checked os.Remove.
func (r *rooted) remove(abs string) error {
	rel, err := r.rel(abs)
	if err != nil {
		return err
	}
	if r.plan.active() {
		// Mirror os.Remove's own semantics: removing an absent target is not
		// a removal. The real (non-plan) root.Remove below returns an
		// ENOENT-flavored error for a manifest-listed file the operator
		// already deleted by hand, and reconcile.go's removal loop does NOT
		// count that as a removal — a plan that unconditionally recorded the
		// remove regardless of whether anything is actually there named a
		// deletion the apply would not perform (B2). r.stat, not r.root.Stat
		// directly, so this stays safe when r.root is nil (planning against a
		// boundary that doesn't exist yet — see openRootedPlannedAbsent):
		// every target is then correctly reported as absent, never removed.
		if _, err := r.stat(abs); err != nil {
			return err
		}
		r.plan.recordRemove(abs)
		return nil
	}
	return r.root.Remove(rel)
}

// existingPerm returns the current file's permission bits, or def when the file
// does not exist (or its resolution escapes the root). Mirrors adapters.go's
// free existingPerm, but through the root so a symlinked target can't be
// stat'd out of bounds.
func (r *rooted) existingPerm(abs string, def os.FileMode) os.FileMode {
	if fi, err := r.stat(abs); err == nil {
		return fi.Mode().Perm()
	}
	return def
}

// writeAtomic is the root-contained twin of writeFileAtomic: it keeps the same
// temp→chmod→fsync→close→rename sequence and mode-preservation contract, but
// every step runs through the os.Root, so an intermediate symlinked directory
// (or an escaping leaf symlink) makes the write fail instead of landing bytes
// outside the boundary. The stale-temp self-heal is identical and equally safe:
// every mutating run holds the exclusive run lock (see AcquireLock), so no
// concurrent run's in-flight temp can be swept.
func (r *rooted) writeAtomic(abs string, data []byte, perm os.FileMode) error {
	rel, err := r.rel(abs)
	if err != nil {
		return err
	}
	if r.plan.active() {
		_, statErr := r.stat(abs)
		r.plan.recordWrite(abs, statErr != nil)
		return nil
	}
	dir, base := filepath.Dir(rel), filepath.Base(rel)
	if dir != "." {
		if err := r.root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("atomic write: mkdir: %w", err)
		}
	}
	r.cleanStaleTemps(dir, base)

	tmpRel := filepath.Join(dir, base+".tmp-"+randSuffix())
	tmp, err := r.root.OpenFile(tmpRel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("atomic write: create temp: %w", err)
	}
	// Best-effort cleanup if we bail before the rename; a no-op after success.
	defer func() { _ = r.root.Remove(tmpRel) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("atomic write: write: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil { // OpenFile made 0600
		_ = tmp.Close()
		return fmt.Errorf("atomic write: chmod: %w", err)
	}
	if err := tmp.Sync(); err != nil { // durability before the rename makes it visible
		_ = tmp.Close()
		return fmt.Errorf("atomic write: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomic write: close: %w", err)
	}
	if err := r.root.Rename(tmpRel, rel); err != nil {
		return fmt.Errorf("atomic write: rename: %w", err)
	}
	return nil
}

// cleanStaleTemps removes any base.tmp-* left in dir by a prior crashed run.
// Uses the root's directory listing so it, too, is contained. A missing/unlisted
// dir is a no-op — the same tolerance the free writeFileAtomic's Glob gives.
func (r *rooted) cleanStaleTemps(dir, base string) {
	if r.plan.active() {
		return // sweeping temps is a mutation
	}
	d, err := r.root.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return
	}
	prefix := base + ".tmp-"
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			_ = r.root.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// randSuffix returns an unpredictable temp-file suffix, mirroring os.CreateTemp's
// randomness (os.Root has no CreateTemp, so the name is generated here).
func randSuffix() string {
	var b [9]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail in practice
	return hex.EncodeToString(b[:])
}

// rootCache lazily opens and memoizes one os.Root per boundary directory, so a
// reconciler touching many targets across claudeDir and several project roots
// opens each boundary at most once. Not safe for concurrent use; a single sync
// run is single-goroutine.
type rootCache struct {
	roots map[string]*rooted // key: cleaned boundary dir
	gone  map[string]bool    // boundaries confirmed missing (openRootedOptional → false)

	// plan, when non-nil, is stamped onto every *rooted this cache hands out —
	// including ones it memoized before a caller asked about a given boundary.
	// Setting the mode only where a root is opened directly (rather than here,
	// at construction) would leave every CACHED root writing for real: get()
	// returns the same *rooted on every call for a boundary, so the mode has to
	// live on the cache itself, not be threaded in per-call.
	plan *Plan
}

func newRootCache() *rootCache {
	return newRootCachePlanned(nil)
}

// newRootCachePlanned is newRootCache with every root it hands out carrying
// plan's mode (nil plan is the real write path, identical to newRootCache).
func newRootCachePlanned(plan *Plan) *rootCache {
	return &rootCache{roots: map[string]*rooted{}, gone: map[string]bool{}, plan: plan}
}

// get returns the (cached) root for dir. ok=false means the directory does not
// exist (nothing to contain there); err covers any other open failure.
func (c *rootCache) get(dir string) (r *rooted, ok bool, err error) {
	clean := filepath.Clean(dir)
	if r, hit := c.roots[clean]; hit {
		return r, true, nil
	}
	if c.gone[clean] {
		return nil, false, nil
	}
	r, ok, err = openRootedOptionalPlanned(clean, c.plan)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		c.gone[clean] = true
		return nil, false, nil
	}
	c.roots[clean] = r
	return r, true, nil
}

// seed pre-populates the cache for dir with an already-constructed root,
// bypassing the normal open. Used only to hand ReconcileSeeds a nil-backed
// *rooted (openRootedPlannedAbsent) for claudeDir when planning against a
// sync root that doesn't exist yet: a real run's MkdirAll guarantees
// claudeDir exists before any get() call for it, and seed mirrors that
// guarantee for the cache without ever touching disk. Overwrites any prior
// entry for dir unconditionally — callers are expected to call this before
// the boundary is ever looked up, not after.
func (c *rootCache) seed(dir string, r *rooted) {
	c.roots[filepath.Clean(dir)] = r
}

// closeAll releases every cached root.
func (c *rootCache) closeAll() {
	for _, r := range c.roots {
		r.Close()
	}
}
