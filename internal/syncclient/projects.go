package syncclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Managed project list for project-scope seed targeting (Slice B §7.4).
// Persisted separately from Config (which is env-derived and never written):
// only credentials.json and this file live on disk.

// Project is a registered project root and the tags the DEVELOPER gave it.
// Tags are purely local: orbeat never learns them. A rule declares which tags
// it is for (migration 00024) and the client writes it only into projects that
// carry one, so the admin says what KIND of project a rule targets and the
// developer says what kind theirs are. Nothing about one machine's filesystem
// has to be modelled server-side for the two to meet.
type Project struct {
	Path string   `json:"path"`
	Tags []string `json:"tags,omitempty"`
}

// projectsFile is the on-disk shape. Entries are decoded permissively because
// a v1 file (a bare array of path strings, every install before tagging) must
// keep working untouched: entry.UnmarshalJSON accepts both forms. It is written
// back in the object form only once something actually needs tags, so a client
// that never tags anything never rewrites the file into a shape an older binary
// cannot read.
type projectsFile struct {
	Projects []entry `json:"projects"`
}

// entry decodes either "path" (v1) or {"path":…,"tags":[…]} (v2).
type entry Project

func (e *entry) UnmarshalJSON(b []byte) error {
	var path string
	if err := json.Unmarshal(b, &path); err == nil {
		e.Path, e.Tags = path, nil
		return nil
	}
	var obj struct {
		Path string   `json:"path"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	if obj.Path == "" {
		return fmt.Errorf("project entry has no path")
	}
	e.Path, e.Tags = obj.Path, obj.Tags
	return nil
}

// MarshalJSON writes the v1 string form for an untagged project and the v2
// object only for a tagged one. That is what keeps a file this binary wrote
// readable by a binary that predates tagging, for every project that has no
// tags to lose.
func (e entry) MarshalJSON() ([]byte, error) {
	if len(e.Tags) == 0 {
		return json.Marshal(e.Path)
	}
	return json.Marshal(struct {
		Path string   `json:"path"`
		Tags []string `json:"tags"`
	}{e.Path, e.Tags})
}

// DefaultProjectsPath is ~/.config/orbeat/projects.json — a sibling of the
// Slice-A token store credentials.json, in the same 0700 directory.
func DefaultProjectsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("projects path: %w", err)
	}
	return filepath.Join(home, ".config", "orbeat", "projects.json"), nil
}

// validProjectPath shape-checks one registered project entry before it is
// ever handed to a reconciler, doctor, or resolveContained — the same
// defense-in-depth this codebase already applies to the sync manifest's own
// user-editable ledgers (validRulesPath, validSeedPath, validGlobalRulesPath).
// projects.json is exactly that kind of file: AddProject always writes an
// absolute, filepath.Clean'd path, so anything else did not come from this
// client's own write path (B25).
//
// A relative entry is the quiet failure mode: nothing here rejects it at
// write time (there is none — this is a read), and every downstream consumer
// resolves it against whatever directory the CLI happens to be invoked from,
// so the SAME projects.json manages a different (or no) tree depending on
// cwd, silently, with no error either way. "." is the sharp case:
// resolveContained treats it as the project root itself, so every governed
// write it guards reads as an escape and the WHOLE sync aborts fatally,
// naming a path that has nothing to do with the actual mistake.
func validProjectPath(p string) bool {
	return p != "" && filepath.IsAbs(p) && p == filepath.Clean(p)
}

// LoadProjects reads the registered projects (empty when the file is absent),
// in both the v1 (bare path strings) and v2 (path plus tags) on-disk forms.
// A shape-invalid entry (see validProjectPath) is dropped rather than
// returned or turned into a load error: one hand-edited or corrupted line
// must not disable project management for every OTHER, valid entry, mirroring
// how a malformed Rules/Seeds ledger line is skipped rather than aborting the
// whole sync.
func LoadProjects(path string) ([]Project, error) {
	valid, _, err := loadProjectsWithInvalid(path)
	return valid, err
}

// loadProjectsWithInvalid is LoadProjects's introspective twin: same read,
// same parse, same per-entry validProjectPath filter, same valid-entry
// return, but it ALSO reports the raw path string of every entry that filter
// dropped. LoadProjects's ordinary callers (sync, project list) have never
// needed that second value, and giving them one they'd have to ignore is how
// a function picks up a signature every caller but one discards; this exists
// so doctor's own check (checkProjectsFile) can warn about a dropped entry
// without LoadProjects itself changing shape. LoadProjects is defined in
// terms of THIS function, not the other way around, so the two cannot drift:
// there is exactly one place that decides what "valid" means for an entry.
func loadProjectsWithInvalid(path string) (valid []Project, invalid []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("load projects: %w", err)
	}
	var f projectsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("load projects: parse: %w", err)
	}
	valid = make([]Project, 0, len(f.Projects))
	for _, e := range f.Projects {
		if !validProjectPath(e.Path) {
			invalid = append(invalid, e.Path)
			continue
		}
		valid = append(valid, Project(e))
	}
	return valid, invalid, nil
}

// ProjectPaths drops the tags. Every consumer that walks project roots without
// caring what kind of project they are (seeds, doctor, the rules writer's
// liveness scan) takes this rather than a second loader, so there is one reader
// of the file and one place where its two on-disk shapes are understood.
func ProjectPaths(ps []Project) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Path)
	}
	return out
}

// saveProjects writes the list atomically. The parent dir is created 0700
// first (it also holds credentials.json), so writeFileAtomic's 0755 MkdirAll
// is a no-op that never widens it.
func saveProjects(path string, list []Project) error {
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save projects: mkdir: %w", err)
	}
	entries := make([]entry, 0, len(list))
	for _, p := range list {
		entries = append(entries, entry(p))
	}
	data, err := json.MarshalIndent(projectsFile{Projects: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("save projects: marshal: %w", err)
	}
	return writeFileAtomic(path, data, 0o644)
}

// AddProject registers an absolute, existing directory (stored cleaned;
// idempotent). Returns the cleaned absolute path that was stored.
func AddProject(path, proj string, tags []string) (string, error) {
	abs, err := filepath.Abs(proj)
	if err != nil {
		return "", fmt.Errorf("add project: %w", err)
	}
	abs = filepath.Clean(abs)
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("add project: %s: %w", abs, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("add project: %s is not a directory", abs)
	}
	tags, err = normalizeTags(tags)
	if err != nil {
		return "", fmt.Errorf("add project: %w", err)
	}
	list, err := LoadProjects(path)
	if err != nil {
		return "", err
	}
	// Re-adding a project REPLACES its tags rather than merging them, and
	// re-adding with no tags clears them. Merging would make removing a tag
	// impossible without deregistering the project, which would strip its
	// managed blocks and re-add them on the next sync: a destructive way to
	// spell an edit.
	for i, p := range list {
		if p.Path == abs {
			list[i].Tags = tags
			return abs, saveProjects(path, list)
		}
	}
	return abs, saveProjects(path, append(list, Project{Path: abs, Tags: tags}))
}

// projectTagRe is the same slug shape the API validates a rule's targetTags
// against (internal/api/admin_artifacts.go). Both ends check it: a tag that
// only one side accepts is a tag that silently matches nothing.
var projectTagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// maxProjectTags mirrors the server's maxTargetTags. A project carrying more
// tags than a rule may name is not an error in itself, but the same ceiling on
// both sides keeps the two halves of one feature describable by one number.
const maxProjectTags = 16

// normalizeTags sorts and de-duplicates, and rejects anything that is not a
// slug. Sorting makes the file stable across re-adds in a different order,
// which matters because it is a file a developer reads and diffs.
func normalizeTags(tags []string) ([]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if !projectTagRe.MatchString(t) {
			return nil, fmt.Errorf("tag %q must be a slug (lowercase, digits, dashes)", t)
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > maxProjectTags {
		return nil, fmt.Errorf("at most %d tags per project, got %d", maxProjectTags, len(out))
	}
	sort.Strings(out)
	return out, nil
}

// ResolveRegisteredProject is the read-only half of RemoveProject: it
// resolves proj to its cleaned absolute path and reports whether it is
// currently registered, WITHOUT writing anything. It exists so a caller that
// must strip a project's governed blocks BEFORE de-registering it (B24 — see
// RemoveProject's own doc comment) can learn abs/found ahead of attempting
// that strip, instead of RemoveProject's write already having happened by the
// time the strip is even tried.
func ResolveRegisteredProject(path, proj string) (string, bool, error) {
	abs, err := filepath.Abs(proj)
	if err != nil {
		return "", false, fmt.Errorf("remove project: %w", err)
	}
	abs = filepath.Clean(abs)
	list, err := LoadProjects(path)
	if err != nil {
		return abs, false, err
	}
	for _, p := range list {
		if p.Path == abs {
			return abs, true, nil
		}
	}
	return abs, false, nil
}

// RemoveProject unregisters proj — the WRITE half. Returns the cleaned path
// and whether it was registered.
//
// CALLERS MUST STRIP THE PROJECT'S GOVERNED BLOCKS BEFORE CALLING THIS
// (B24), not after: once a project drops out of the registered set, its
// Rules/Seeds ledger entries stop being trusted (trustedSeedBoundary and its
// rules-side counterpart, B23's fix) and are preserved-but-never-touched by
// every future ordinary sync rather than stripped — there is no "the next
// sync's ledger-driven pass self-heals" to fall back on if the strip is
// skipped, partially completes, or is attempted after this call instead of
// before it. ResolveRegisteredProject is the read-only half of this contract:
// use it to learn abs/found ahead of the strip, then call this function only
// once the strip has actually succeeded (see cmd/orbeat-sync's `project
// remove` handler).
func RemoveProject(path, proj string) (string, bool, error) {
	abs, found, err := ResolveRegisteredProject(path, proj)
	if err != nil || !found {
		return abs, found, err
	}
	list, err := LoadProjects(path)
	if err != nil {
		return abs, false, err
	}
	kept := make([]Project, 0, len(list))
	for _, p := range list {
		if p.Path == abs {
			continue
		}
		kept = append(kept, p)
	}
	return abs, true, saveProjects(path, kept)
}
