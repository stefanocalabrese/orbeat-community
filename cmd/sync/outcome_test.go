package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/syncclient"
)

// TestRenderJSONNeverRanIsNullRanEmptyIsArray pins the distinction the schema
// exists for. A section whose reconciler never ran (fatal abort upstream) must
// be null; one that ran and found nothing must be zero counts with [] slices.
// Asserting only "it parses" would pass on a payload that is valid JSON and
// carries the wrong meaning.
func TestRenderJSONNeverRanIsNullRanEmptyIsArray(t *testing.T) {
	o := &syncOutcome{
		ExitCode:  2,
		Artifacts: &artifactsSection{Skipped: strs(nil), Warnings: strs(nil), Failures: strs(nil)},
		// Seeds and Rules deliberately nil: they never ran.
	}
	var buf bytes.Buffer
	if err := renderJSON(&buf, o); err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if got["seeds"] != nil {
		t.Errorf("seeds must be null when the reconciler never ran, got %#v", got["seeds"])
	}
	if got["rules"] != nil {
		t.Errorf("rules must be null when the reconciler never ran, got %#v", got["rules"])
	}
	arts, ok := got["artifacts"].(map[string]any)
	if !ok {
		t.Fatalf("artifacts must be an object, got %#v", got["artifacts"])
	}
	for _, k := range []string{"skipped", "warnings", "failures"} {
		v, ok := arts[k].([]any)
		if !ok {
			t.Errorf("artifacts.%s must be [] not null, got %#v", k, arts[k])
			continue
		}
		if len(v) != 0 {
			t.Errorf("artifacts.%s should be empty, got %v", k, v)
		}
	}
	if got["exitCode"] != float64(2) {
		t.Errorf("exitCode = %v, want 2", got["exitCode"])
	}
}

// TestRenderJSONAllSevenSliceFieldsAreArrays covers every slice in the schema,
// not just the ones the fatal-path case happens to touch. A review found the
// narrower wording would pass on a payload with six correct fields and one null.
func TestRenderJSONAllSevenSliceFieldsAreArrays(t *testing.T) {
	o := &syncOutcome{
		Artifacts: &artifactsSection{Skipped: strs(nil), Warnings: strs(nil), Failures: strs(nil)},
		Seeds:     &blocksSection{Warnings: strs(nil), Failures: strs(nil)},
		Rules:     &blocksSection{Warnings: strs(nil), Failures: strs(nil)},
	}
	var buf bytes.Buffer
	if err := renderJSON(&buf, o); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][2]string{
		{"artifacts", "skipped"}, {"artifacts", "warnings"}, {"artifacts", "failures"},
		{"seeds", "warnings"}, {"seeds", "failures"},
		{"rules", "warnings"}, {"rules", "failures"},
	} {
		sec, ok := got[path[0]].(map[string]any)
		if !ok {
			t.Errorf("%s must be an object, got %#v", path[0], got[path[0]])
			continue
		}
		if _, ok := sec[path[1]].([]any); !ok {
			t.Errorf("%s.%s must be [] not null, got %#v", path[0], path[1], sec[path[1]])
		}
	}
}

// TestRenderJSONEmitsExactlyOneObject is the stdout-purity gate at the renderer
// level. NOTE it writes to a buffer, so it CANNOT catch a stray fmt.Println to
// os.Stdout; only Task 4's real-binary smoke gate can. Do not read this as
// covering that.
func TestRenderJSONEmitsExactlyOneObject(t *testing.T) {
	o := &syncOutcome{ExitCode: 0,
		Artifacts: &artifactsSection{Skipped: strs(nil), Warnings: strs(nil), Failures: strs(nil)},
		Seeds:     &blocksSection{Warnings: strs(nil), Failures: strs(nil)},
		Rules:     &blocksSection{Warnings: strs(nil), Failures: strs(nil)},
	}
	var buf bytes.Buffer
	if err := renderJSON(&buf, o); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(buf.String()))
	var first map[string]any
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("first value did not decode: %v", err)
	}
	if dec.More() {
		t.Fatalf("stdout carried more than one JSON value:\n%s", buf.String())
	}
}

// TestRenderJSONFatalIsSurfaced proves the fatal message reaches the payload,
// not only stderr. A consumer on the exit-2 path has nothing else to read.
func TestRenderJSONFatalIsSurfaced(t *testing.T) {
	msg := "manifest: integrity check failed"
	o := &syncOutcome{ExitCode: 2, Fatal: &msg,
		Artifacts: &artifactsSection{Skipped: strs(nil), Warnings: strs(nil), Failures: strs(nil)}}
	var buf bytes.Buffer
	if err := renderJSON(&buf, o); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["fatal"] != msg {
		t.Errorf("fatal = %#v, want %q", got["fatal"], msg)
	}
}

// TestRenderHumanGolden pins the human output byte-for-byte. Spec §8 lists
// "changing the human output" as a non-goal, and a review found that non-goal
// was enforced by NOTHING: mutating "Synced %d artifact(s):" to "MUTATED ..."
// left the whole package green. This is a stable CLI surface.
func TestRenderHumanGolden(t *testing.T) {
	o := &syncOutcome{
		Artifacts: &artifactsSection{
			Handled: 3, Added: 1, Updated: 1, Unchanged: 1, Removed: 0,
			Skipped:  []string{"skills/collide/SKILL.md"},
			Warnings: []string{"unknown type"},
			Failures: []string{"agents/a.md: permission denied"},
		},
		Seeds:           &blocksSection{Written: 2, Unchanged: 1, Stripped: 0, Warnings: strs(nil), Failures: strs(nil)},
		Rules:           &blocksSection{Written: 1, Unchanged: 0, Stripped: 1, Warnings: strs(nil), Failures: []string{"/p: unwritable"}},
		RestartRequired: true,
	}
	want := "Synced 3 artifact(s): 1 added, 1 updated, 1 unchanged, 0 removed.\n" +
		"  skipped (a non-orbeat file already exists): skills/collide/SKILL.md\n" +
		"  warning: unknown type\n" +
		"  failed: agents/a.md: permission denied\n" +
		"Seeds: 2 written, 1 unchanged, 0 stripped.\n" +
		"Rules: 1 written, 0 unchanged, 1 stripped.\n" +
		"  failed: /p: unwritable\n" +
		"Agent set changed — restart Claude Code (or start a new prompt) to pick up the change.\n"
	var buf bytes.Buffer
	renderHuman(&buf, o)
	if got := buf.String(); got != want {
		t.Errorf("renderHuman output changed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestOutcomeJSONCarriesDryRunAndChanges(t *testing.T) {
	o := &syncOutcome{
		ExitCode: 0, DryRun: true,
		Artifacts: &artifactsSection{
			Added:   1,
			Changes: []syncclient.Change{{Path: "/h/.claude/agents/a.md", Op: syncclient.OpCreate}},
		},
	}
	var buf bytes.Buffer
	renderJSON(&buf, o)

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if got["dryRun"] != true {
		t.Errorf("dryRun must be true, got %v", got["dryRun"])
	}
	arts := got["artifacts"].(map[string]any)
	if _, ok := arts["added"]; !ok {
		t.Error("existing counters must survive — smoke F1/F4 assert on them")
	}
	ch := arts["changes"].([]any)
	if len(ch) != 1 || ch[0].(map[string]any)["op"] != "create" {
		t.Errorf("changes must carry path+op, got %+v", ch)
	}
}

func TestHumanRenderMarksADryRun(t *testing.T) {
	o := &syncOutcome{ExitCode: 0, DryRun: true, Artifacts: &artifactsSection{Added: 1,
		Changes: []syncclient.Change{{Path: "/h/.claude/agents/a.md", Op: syncclient.OpCreate}}}}
	var buf bytes.Buffer
	renderHuman(&buf, o)
	out := buf.String()
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("a dry run must be unmistakable in human output, got:\n%s", out)
	}
	if !strings.Contains(out, "agents/a.md") {
		t.Errorf("per-item output must name the path, got:\n%s", out)
	}
}

// TestSectionChangesFiltersTheManifestBookkeepingEntry drives a REAL
// syncclient.Reconcile in plan mode (not a fabricated Change slice) so the
// manifest write is genuinely recorded — Reconcile calls saveManifest
// unconditionally, and saveManifest routes through the same guarded
// writeAtomic as any other write, so a Plan always carries the ledger entry
// alongside any real one. sectionChanges must drop exactly that entry: the
// manifest is bookkeeping, not a user-facing change (spec fact #1).
func TestSectionChangesFiltersTheManifestBookkeepingEntry(t *testing.T) {
	claudeDir := t.TempDir()
	plan := &syncclient.Plan{}
	if _, err := syncclient.Reconcile(claudeDir,
		[]syncclient.Artifact{{Type: "subagent", Name: "a", Content: "hi"}}, plan); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	raw := plan.Changes()
	manifestPath := filepath.Join(claudeDir, ".orbeat-sync-manifest.json")
	sawManifest := false
	for _, c := range raw {
		if c.Path == manifestPath {
			sawManifest = true
		}
	}
	if !sawManifest {
		t.Fatalf("precondition failed: the manifest write was never recorded, so this test cannot exercise the filter: %+v", raw)
	}

	got := sectionChanges(plan)
	for _, c := range got {
		if filepath.Base(c.Path) == ".orbeat-sync-manifest.json" {
			t.Fatalf("the manifest bookkeeping entry leaked into the user-facing report: %+v", got)
		}
	}
	if len(got) != len(raw)-1 {
		t.Fatalf("sectionChanges must drop exactly the manifest entry: raw=%d got=%d\nraw=%+v\ngot=%+v", len(raw), len(got), raw, got)
	}
}

func TestNonDryRunOutputIsUnchanged(t *testing.T) {
	// The default path must be byte-for-byte what it was. Build an outcome with
	// no DryRun and no Changes and assert the rendered text contains no dry-run
	// artifacts at all.
	o := &syncOutcome{ExitCode: 0, Artifacts: &artifactsSection{Added: 1}}
	var buf bytes.Buffer
	renderHuman(&buf, o)
	if strings.Contains(buf.String(), "DRY RUN") {
		t.Errorf("a normal run must not mention a dry run:\n%s", buf.String())
	}
}
