package syncclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadProjectsReadsV1AndV2 pins the on-disk compatibility both directions.
// Every install that predates tagging has a v1 file (a bare array of path
// strings), and a binary that predates tagging must keep reading whatever this
// one writes for an untagged project. The mixed fixture is the point: a single
// file can hold both forms while a developer tags one project and not another.
func TestLoadProjectsReadsV1AndV2(t *testing.T) {
	dir := t.TempDir()
	pj := filepath.Join(dir, "projects.json")
	must(t, os.WriteFile(pj, []byte(`{"projects":["/a",{"path":"/b","tags":["go","api"]}]}`), 0o644))

	got, err := LoadProjects(pj)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 projects, got %+v", got)
	}
	if got[0].Path != "/a" || len(got[0].Tags) != 0 {
		t.Fatalf("v1 entry decoded as %+v", got[0])
	}
	if got[1].Path != "/b" || strings.Join(got[1].Tags, ",") != "go,api" {
		t.Fatalf("v2 entry decoded as %+v", got[1])
	}
}

// TestSaveProjectsKeepsUntaggedEntriesAsStrings is the other half: writing must
// not silently upgrade the file format for projects that gained nothing from
// it, or a developer who tags one project makes every OTHER project unreadable
// to an older orbeat-sync.
func TestSaveProjectsKeepsUntaggedEntriesAsStrings(t *testing.T) {
	dir := t.TempDir()
	pj := filepath.Join(dir, "projects.json")
	must(t, saveProjects(pj, []Project{{Path: "/a"}, {Path: "/b", Tags: []string{"go"}}}))

	raw, err := os.ReadFile(pj)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Projects []json.RawMessage `json:"projects"`
	}
	must(t, json.Unmarshal(raw, &probe))
	if len(probe.Projects) != 2 {
		t.Fatalf("want 2 entries, got %s", raw)
	}
	if string(probe.Projects[0]) != `"/a"` {
		t.Fatalf("an untagged project must stay a bare string, got %s", probe.Projects[0])
	}
	if !strings.Contains(string(probe.Projects[1]), `"tags"`) {
		t.Fatalf("a tagged project must carry its tags, got %s", probe.Projects[1])
	}
}

// TestAddProjectReplacesTags pins that re-adding REPLACES rather than merges,
// including clearing. Merging would leave no way to remove a tag except
// deregistering the project, which strips its managed blocks: a destructive way
// to spell an edit.
func TestAddProjectReplacesTags(t *testing.T) {
	dir := t.TempDir()
	pj := filepath.Join(dir, "projects.json")
	proj := t.TempDir()

	if _, err := AddProject(pj, proj, []string{"go", "api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := AddProject(pj, proj, []string{"rust"}); err != nil {
		t.Fatal(err)
	}
	list, _ := LoadProjects(pj)
	if len(list) != 1 || strings.Join(list[0].Tags, ",") != "rust" {
		t.Fatalf("re-add must replace the tag set, got %+v", list)
	}
	if _, err := AddProject(pj, proj, nil); err != nil {
		t.Fatal(err)
	}
	list, _ = LoadProjects(pj)
	if len(list) != 1 || len(list[0].Tags) != 0 {
		t.Fatalf("re-add with no tags must clear them, got %+v", list)
	}
}

// TestAddProjectRejectsBadTags keeps the client's tag shape identical to the
// server's targetTags rule. A tag only one side accepts is a tag that silently
// matches nothing, which is indistinguishable from a rule that simply does not
// apply, and that is the worst possible failure for a governance feature.
func TestAddProjectRejectsBadTags(t *testing.T) {
	dir := t.TempDir()
	pj := filepath.Join(dir, "projects.json")
	proj := t.TempDir()

	for _, bad := range [][]string{{"Go"}, {"has space"}, {"-leading"}, {"trailing_"}, {""}} {
		if _, err := AddProject(pj, proj, bad); err == nil {
			t.Fatalf("tag %q was accepted", bad)
		}
	}
	many := make([]string, 17)
	for i := range many {
		many[i] = string(rune('a'+i)) + "tag"
	}
	if _, err := AddProject(pj, proj, many); err == nil {
		t.Fatal("17 tags were accepted; the ceiling is 16")
	}
	if list, _ := LoadProjects(pj); len(list) != 0 {
		t.Fatalf("a rejected add must not register the project: %+v", list)
	}
}
