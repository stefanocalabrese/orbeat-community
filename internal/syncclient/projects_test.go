package syncclient

import (
	"os"
	"path/filepath"
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
	if _, err := AddProject(pj, projA); err != nil {
		t.Fatalf("add A: %v", err)
	}
	if _, err := AddProject(pj, projB); err != nil {
		t.Fatalf("add B: %v", err)
	}
	if _, err := AddProject(pj, projA); err != nil {
		t.Fatalf("re-add A: %v", err)
	}
	list, _ = LoadProjects(pj)
	if len(list) != 2 {
		t.Fatalf("want 2, got %v", list)
	}

	// A non-existent dir is rejected.
	if _, err := AddProject(pj, filepath.Join(dir, "nope")); err == nil {
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
	if len(list) != 1 || list[0] != projB {
		t.Fatalf("want only B, got %v", list)
	}

	// The config dir must be private (0700), like the token store's.
	st, err := os.Stat(filepath.Dir(pj))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("config dir perms: %v %v", err, st.Mode())
	}
}
