package syncclient

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestProjectsAddRemoveList(t *testing.T) {
	dir := t.TempDir()
	pj := filepath.Join(dir, "cfg", "projects.json")
	projA := filepath.Join(dir, "proj-a")
	projB := filepath.Join(dir, "proj-b")
	_ = os.MkdirAll(projA, 0o755)
	_ = os.MkdirAll(projB, 0o755)

	// Empty state: no file → empty list, no error.
	list, err := LoadProjects(pj)
	if err != nil || len(list) != 0 {
		t.Fatalf("empty load: %v %v", err, list)
	}

	// Add both; adding twice is idempotent.
	if _, err := AddProject(pj, projA, nil); err != nil {
		t.Fatalf("add A: %v", err)
	}
	if _, err := AddProject(pj, projB, nil); err != nil {
		t.Fatalf("add B: %v", err)
	}
	if _, err := AddProject(pj, projA, nil); err != nil {
		t.Fatalf("re-add A: %v", err)
	}
	list, _ = LoadProjects(pj)
	if len(list) != 2 {
		t.Fatalf("want 2, got %v", list)
	}

	// A non-existent dir is rejected.
	if _, err := AddProject(pj, filepath.Join(dir, "nope"), nil); err == nil {
		t.Fatalf("want error for missing dir")
	}

	// Remove: reports found; unknown path reports !found.
	abs, found, err := RemoveProject(pj, projA)
	if err != nil || !found || abs != projA {
		t.Fatalf("remove: %v found=%v abs=%q", err, found, abs)
	}
	_, found, err = RemoveProject(pj, projA)
	if err != nil || found {
		t.Fatalf("second remove must be !found: %v %v", err, found)
	}
	list, _ = LoadProjects(pj)
	if len(list) != 1 || list[0].Path != projB {
		t.Fatalf("want only B, got %v", list)
	}

	// The config dir must be private (0700), like the token store's.
	st, err := os.Stat(filepath.Dir(pj))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("config dir perms: %v %v", err, st.Mode())
	}
}

// B25: projects.json is a user-editable file (AddProject always writes an
// absolute, filepath.Clean'd path, so anything else did not come from this
// client's own write path), and every reconciler that consumes LoadProjects'
// output resolves derived paths under each entry as if it were an absolute
// project root. A relative entry silently resolves against whatever
// directory the CLI happens to be invoked from — a different (or no) tree
// depending on cwd, with no error either way — and "." is worse: it makes
// resolveContained treat the sync/project root itself as the escape boundary,
// which aborts the WHOLE sync fatally, naming a path that has nothing to do
// with the actual mistake. Both must be dropped before they ever reach a
// reconciler, exactly like a shape-invalid entry in the Rules/Seeds ledgers.
func TestLoadProjectsDropsShapeInvalidEntries(t *testing.T) {
	dir := t.TempDir()
	pj := filepath.Join(dir, "cfg", "projects.json")
	must(t, os.MkdirAll(filepath.Dir(pj), 0o700))

	goodA := filepath.Join(dir, "proj-a")
	goodB := filepath.Join(dir, "proj-b")
	must(t, os.MkdirAll(goodA, 0o755))
	must(t, os.MkdirAll(goodB, 0o755))

	raw := `{"projects":["` + goodA + `", "` + goodB + `", ".", "relative/path", "` + goodB + `/../proj-b/", ""]}`
	must(t, os.WriteFile(pj, []byte(raw), 0o644))

	list, err := LoadProjects(pj)
	if err != nil {
		t.Fatalf("a shape-invalid entry must be dropped, not turned into a load error: %v", err)
	}
	got := ProjectPaths(list)
	sort.Strings(got)
	want := []string{goodA, goodB}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("LoadProjects = %v, want exactly the two absolute+clean entries %v (relative, \".\", a non-clean path, and an empty path must all be dropped)", got, want)
	}
}
