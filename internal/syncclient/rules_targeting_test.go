package syncclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRulesFor pins the matching model itself: intersection, with an empty
// target set meaning "every project" rather than "no project".
//
// That asymmetry is the whole compatibility story. A rule written before
// migration 00024 has no tags, an older server sends no `targetTags` field at
// all, and a project registered before `--tag` existed has no tags either. All
// three of those have to keep behaving exactly as they did, and they do because
// the empty set is the permissive one on the rule side and the restrictive one
// on the project side.
func TestRulesFor(t *testing.T) {
	universal := Artifact{Type: "rule", Name: "universal"}
	goRule := Artifact{Type: "rule", Name: "go-rule", TargetTags: []string{"go"}}
	polyRule := Artifact{Type: "rule", Name: "poly", TargetTags: []string{"go", "rust"}}
	all := []Artifact{universal, goRule, polyRule}

	for _, tc := range []struct {
		name string
		tags []string
		want []string
	}{
		{"untagged project gets untargeted rules only", nil, []string{"universal"}},
		{"a matching tag adds the targeted rule", []string{"go"}, []string{"universal", "go-rule", "poly"}},
		{"any one tag of a multi-tag rule is enough", []string{"rust"}, []string{"universal", "poly"}},
		{"a non-matching tag changes nothing", []string{"python"}, []string{"universal"}},
		{"extra tags do not duplicate a rule", []string{"go", "rust"}, []string{"universal", "go-rule", "poly"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rulesFor(all, tc.tags)
			var names []string
			for _, r := range got {
				names = append(names, r.Name)
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("tags %v matched %v, want %v", tc.tags, names, tc.want)
			}
		})
	}
}

// TestReconcileRulesTargetsByProjectTag drives the real reconciler over two
// projects that differ only in their tags, which is what makes the assertion
// discriminating: a filter that did nothing would leave both files identical,
// and every "the rule is present" assertion would still pass.
func TestReconcileRulesTargetsByProjectTag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	goProj, otherProj := t.TempDir(), t.TempDir()

	rules := []Artifact{
		{Type: "rule", Name: "everywhere", Content: "EVERYWHERE-BODY"},
		{Type: "rule", Name: "go-only", Content: "GO-ONLY-BODY", TargetTags: []string{"go"}},
	}
	projects := []Project{{Path: goProj, Tags: []string{"go"}}, {Path: otherProj}}

	if _, err := ReconcileRules(claudeDir, projects, rules, nil); err != nil {
		t.Fatal(err)
	}

	goAgents := readFile(t, filepath.Join(goProj, "AGENTS.md"))
	if !strings.Contains(goAgents, "GO-ONLY-BODY") || !strings.Contains(goAgents, "EVERYWHERE-BODY") {
		t.Fatalf("the go project should carry both rules:\n%s", goAgents)
	}
	otherAgents := readFile(t, filepath.Join(otherProj, "AGENTS.md"))
	if strings.Contains(otherAgents, "GO-ONLY-BODY") {
		t.Fatalf("a go-tagged rule reached an untagged project:\n%s", otherAgents)
	}
	if !strings.Contains(otherAgents, "EVERYWHERE-BODY") {
		t.Fatalf("an untargeted rule failed to reach an untagged project:\n%s", otherAgents)
	}
}

// TestReconcileRulesStripsAProjectThatStopsMatching is the half that makes
// re-targeting a real operation rather than a label. Narrowing a rule has to
// take the block OFF the machines it no longer applies to; leaving the last
// delivered copy frozen on disk would mean an admin could never withdraw an
// instruction from a project, only stop updating it.
func TestReconcileRulesStripsAProjectThatStopsMatching(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	proj := t.TempDir()

	// First sync: the rule is untargeted, so it lands.
	rule := Artifact{Type: "rule", Name: "narrowing", Content: "NARROWING-BODY"}
	if _, err := ReconcileRules(claudeDir, []Project{{Path: proj}}, []Artifact{rule}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, filepath.Join(proj, "AGENTS.md")), "NARROWING-BODY") {
		t.Fatal("precondition failed: the untargeted rule never landed")
	}

	// The admin re-targets it to `go`; this project carries no tags.
	rule.TargetTags = []string{"go"}
	res, err := ReconcileRules(claudeDir, []Project{{Path: proj}}, []Artifact{rule}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stripped == 0 {
		t.Fatalf("re-targeting away from a project must strip its block, got %+v", res)
	}
	agents := readFile(t, filepath.Join(proj, "AGENTS.md"))
	if strings.Contains(agents, "NARROWING-BODY") {
		t.Fatalf("the rule survived a re-target it no longer matches:\n%s", agents)
	}
	if strings.Contains(agents, "ORBEAT-RULES:BEGIN") {
		t.Fatalf("an empty managed block was left behind:\n%s", agents)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
