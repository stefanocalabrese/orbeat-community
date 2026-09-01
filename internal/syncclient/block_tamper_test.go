package syncclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A8: both merges decided "unchanged" by comparing the hash captured out of the
// marker in the file against the hash of the DESIRED body. Nothing ever
// re-hashed the body actually on disk, so an edit inside the governed block
// that left the marker line alone survived every subsequent sync, reported as
// unchanged with no warning.
//
// Every pre-existing merge and strip test drives the unmodified rendered block,
// where the marker and the body agree by construction, so the mutant that
// passes all of them is the shipped code. These tests therefore build the
// body/marker mismatch directly.

// tamperSeedBody rewrites the body of the ORBEAT-SEED block for name, leaving
// the BEGIN marker (and its sha) byte-identical.
func tamperSeedBody(t *testing.T, content, name, newBody string) string {
	t.Helper()
	loc := seedBlockRe(name).FindStringSubmatchIndex(content)
	if loc == nil {
		t.Fatalf("no ORBEAT-SEED block for %q in:\n%s", name, content)
	}
	out := content[:loc[4]] + newBody + "\n" + content[loc[5]:]
	if !strings.Contains(out, content[loc[2]:loc[3]]) {
		t.Fatal("tamper helper lost the marker hash it is supposed to preserve")
	}
	return out
}

func tamperRulesBody(t *testing.T, content, newBody string) string {
	t.Helper()
	loc := rulesBlockRe.FindStringSubmatchIndex(content)
	if loc == nil {
		t.Fatalf("no ORBEAT-RULES block in:\n%s", content)
	}
	return content[:loc[4]] + newBody + "\n" + content[loc[5]:]
}

func TestMergeSeedRehashesTheBodyOnDisk(t *testing.T) {
	const name, body = "rev", "NEVER force-push"
	rendered := renderSeedBlock(name, body)

	t.Run("an edited body with an untouched marker is restored", func(t *testing.T) {
		tampered := tamperSeedBody(t, rendered, name, "force-push is fine")
		if !strings.Contains(tampered, "force-push is fine") {
			t.Fatalf("fixture is not tampered:\n%s", tampered)
		}
		out, changed, restored := mergeSeed(tampered, name, body)
		if !changed || !restored {
			t.Fatalf("changed=%v restored=%v, want both true", changed, restored)
		}
		if out != rendered {
			t.Fatalf("governed body not restored:\n%s", out)
		}
	})

	t.Run("an untampered block is still a no-op", func(t *testing.T) {
		out, changed, restored := mergeSeed(rendered, name, body)
		if changed || restored || out != rendered {
			t.Fatalf("changed=%v restored=%v, want a byte-identical no-op", changed, restored)
		}
	})

	t.Run("an ordinary server-side update is not reported as a restoration", func(t *testing.T) {
		_, changed, restored := mergeSeed(rendered, name, "a new governed body")
		if !changed || restored {
			t.Fatalf("changed=%v restored=%v, want changed without a restoration", changed, restored)
		}
	})

	t.Run("a body edited while the desired body also moved is still reported", func(t *testing.T) {
		tampered := tamperSeedBody(t, rendered, name, "force-push is fine")
		_, changed, restored := mergeSeed(tampered, name, "a third body")
		if !changed || !restored {
			t.Fatalf("changed=%v restored=%v, want both true", changed, restored)
		}
	})
}

func TestMergeRulesRehashesTheBodyOnDisk(t *testing.T) {
	const body = "## sec\n\nNEVER force-push\n"
	rendered := renderRulesBlock(body)

	t.Run("an edited body with an untouched marker is restored", func(t *testing.T) {
		tampered := tamperRulesBody(t, rendered, "## sec\n\nforce-push is fine")
		out, changed, restored := mergeRules(tampered, body)
		if !changed || !restored {
			t.Fatalf("changed=%v restored=%v, want both true", changed, restored)
		}
		if out != rendered {
			t.Fatalf("governed body not restored:\n%s", out)
		}
	})

	t.Run("an untampered block is still a no-op", func(t *testing.T) {
		out, changed, restored := mergeRules(rendered, body)
		if changed || restored || out != rendered {
			t.Fatalf("changed=%v restored=%v, want a byte-identical no-op", changed, restored)
		}
	})

	t.Run("an ordinary server-side update is not reported as a restoration", func(t *testing.T) {
		_, changed, restored := mergeRules(rendered, "## sec\n\nsomething else\n")
		if !changed || restored {
			t.Fatalf("changed=%v restored=%v, want changed without a restoration", changed, restored)
		}
	})
}

// TestReconcileSeedsRestoresAnEditedGovernedBody is A8 end to end, in the shape
// the audit reproduced it: sync, edit the body under the marker, sync again.
// The old behaviour was written=0 unchanged=1 with zero warnings and the edit
// still on disk.
func TestReconcileSeedsRestoresAnEditedGovernedBody(t *testing.T) {
	cd := t.TempDir()
	arts := []Artifact{seedArt("rev", "user", "NEVER force-push")}
	if _, err := ReconcileSeeds(cd, nil, arts, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cd, "agent-memory", "rev", "MEMORY.md")
	before := readFileT(t, target)

	tampered := tamperSeedBody(t, before, "rev", "force-push is fine")
	must(t, os.WriteFile(target, []byte(tampered), 0o644))

	res, err := ReconcileSeeds(cd, nil, arts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 1 || res.Unchanged != 0 {
		t.Fatalf("res = %+v, want written=1 (the edit must not read as unchanged)", res)
	}
	if !warningNaming(res.Warnings, target) {
		t.Fatalf("no warning names %s; warnings = %v", target, res.Warnings)
	}
	got := readFileT(t, target)
	if got != before {
		t.Fatalf("governed body not restored:\n%s", got)
	}
	if strings.Contains(got, "force-push is fine") {
		t.Fatal("the tampered instruction is still on disk")
	}
	// The developer's own notes, outside the block, are untouched by a
	// restoration: only the managed block is rewritten.
	must(t, os.WriteFile(target, []byte(tamperSeedBody(t, before, "rev", "edited again")+"\n- my own note\n"), 0o644))
	if _, err := ReconcileSeeds(cd, nil, arts, nil); err != nil {
		t.Fatal(err)
	}
	if after := readFileT(t, target); !strings.Contains(after, "- my own note") {
		t.Fatalf("a restoration ate the agent's own notes:\n%s", after)
	}
}

// TestReconcileRulesRestoresAnEditedGovernedBody is the same for ORBEAT-RULES
// in a project's AGENTS.md, plus the one thing the rules side can get wrong
// that the seed side cannot: a restoration must NOT suppress the applied
// claim. Routing the notice through the malformed-marker channel would report
// the rule as not deployed at the exact moment its bytes were put back.
func TestReconcileRulesRestoresAnEditedGovernedBody(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	proj := t.TempDir()

	rules := []Artifact{{ID: "rule-1", Revision: 2, Type: "rule", Name: "sec", Content: "NEVER force-push"}}
	if _, err := ReconcileRules(claudeDir, projs(proj), rules, nil); err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(proj, "AGENTS.md")
	before := readFileT(t, agents)

	must(t, os.WriteFile(agents, []byte(tamperRulesBody(t, before, "## sec\n\nforce-push is fine")), 0o644))

	res, err := ReconcileRules(claudeDir, projs(proj), rules, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 1 || res.Unchanged != 0 {
		t.Fatalf("res = %+v, want written=1 (the edit must not read as unchanged)", res)
	}
	if !warningNaming(res.Warnings, agents) {
		t.Fatalf("no warning names %s; warnings = %v", agents, res.Warnings)
	}
	if got := readFileT(t, agents); got != before {
		t.Fatalf("governed body not restored:\n%s", got)
	}
	if len(res.Applied) != 1 || res.Applied[0].ArtifactID != "rule-1" {
		t.Fatalf("Applied = %+v, want the rule still counted as applied after a restoration", res.Applied)
	}
}

// globalOnlyHome points HOME at a fresh temp dir and returns its .claude.
// Moving HOME is what keeps globalRuleTargets down to a single file: a real
// ~/.codex on the machine running the tests would add a second, healthy target,
// and a rule landing there would let the malformed-marker half below pass on
// the very mutant it exists to kill.
func globalOnlyHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	must(t, os.MkdirAll(claudeDir, 0o755))
	return claudeDir
}

// globalRule is the one artifact both halves below drive.
func globalRule() []Artifact {
	return []Artifact{{ID: "g-1", Revision: 4, Type: "rule", Name: "sec", Content: "NEVER force-push", RuleScope: "global"}}
}

// TestReconcileGlobalRulesAppliedIsGatedOnSkipsOnly gates the global-scope copy
// of the "only a malformed-marker skip may withhold the applied claim" rule
// (rules_global.go, `if len(notes.skips) == 0`). That loop is separate code
// from the project pass, so the Applied assertion in
// TestReconcileRulesRestoresAnEditedGovernedBody cannot reach it, and until
// this test existed both mutants of that line left the whole package green.
//
// Both halves are required, because each kills a different mutant:
//
//   - `len(notes.all()) == 0`, the pre-A8 semantics, survives the second half
//     and dies on the first: it drops every global rule out of
//     RulesResult.Applied at the exact moment the governed bytes were put back.
//     cmd/orbeat-sync unions that slice into the deployment-registry report, so
//     a fleet would read as "global rules not deployed" on the run that
//     restored them.
//   - `landed = true` unconditionally deletes the gate. It survives the first
//     half and dies on the second, which is the malformed-marker case the gate
//     was written for in the first place.
func TestReconcileGlobalRulesAppliedIsGatedOnSkipsOnly(t *testing.T) {
	t.Run("a restoration keeps the applied claim", func(t *testing.T) {
		claudeDir := globalOnlyHome(t)
		if _, err := ReconcileRules(claudeDir, nil, globalRule(), nil); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(claudeDir, "CLAUDE.md")
		before := readFileT(t, target)

		must(t, os.WriteFile(target, []byte(tamperRulesBody(t, before, "## sec\n\nforce-push is fine")), 0o644))

		res, err := ReconcileRules(claudeDir, nil, globalRule(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Written != 1 || res.Unchanged != 0 {
			t.Fatalf("res = %+v, want written=1 (the edit must not read as unchanged)", res)
		}
		if !warningNaming(res.Warnings, target) {
			t.Fatalf("fixture broken: no warning names %s; warnings = %v", target, res.Warnings)
		}
		if got := readFileT(t, target); got != before {
			t.Fatalf("governed body not restored:\n%s", got)
		}
		wantApplied(t, res.Applied, AppliedArtifact{ArtifactID: "g-1", Revision: 4})
	})

	t.Run("a malformed marker still withholds it", func(t *testing.T) {
		claudeDir := globalOnlyHome(t)
		target := filepath.Join(claudeDir, "CLAUDE.md")
		orphan := "<!-- ORBEAT-RULES:BEGIN sha=deadbeef0000 x -->\n"
		must(t, os.WriteFile(target, []byte(orphan+"# my own notes\n"), 0o644))

		res, err := ReconcileRules(claudeDir, nil, globalRule(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "malformed") {
			t.Fatalf("fixture broken: want exactly one malformed-marker warning, got %+v", res.Warnings)
		}
		if strings.Contains(readFileT(t, target), "## sec") {
			t.Fatal("fixture broken: the malformed file must not have been spliced")
		}
		wantApplied(t, res.Applied)
	})
}

// restorationNotice is the fragment every A8 warning shares. Asserting on it
// rather than on the whole sentence keeps the gates below independent of the
// wording, which is the part that is allowed to change.
const restorationNotice = "restored the governed content"

func hasRestorationNotice(warnings []string) bool {
	for _, w := range warnings {
		if strings.Contains(w, restorationNotice) {
			return true
		}
	}
	return false
}

// TestFirstWriteClaimsNoRestoration gates the branch A8 does NOT apply to: a
// merge that found no governed block at all, which is every first-ever sync and
// every developer file that carried notes before orbeat-sync touched it.
//
// Forcing restored=true on that branch of mergeSeed left the whole package
// green, because no seed test asserted that an ordinary sync warns about
// nothing. The result would be a false tamper alarm printed against a brand-new
// MEMORY.md, on the one surface whose entire purpose is tamper evidence.
//
// The rules half is here on purpose rather than by luck: mergeRules returns a
// literal false on its two no-block paths, and the only thing standing between
// the same mutant and a green suite today is an unrelated fixture assertion in
// applied_test.go that happens to demand exactly one warning.
func TestFirstWriteClaimsNoRestoration(t *testing.T) {
	noBlock := []string{"", "# my own notes\n", "notes\n\nmore notes\n"}

	t.Run("mergeSeed", func(t *testing.T) {
		for _, existing := range noBlock {
			if _, changed, restored := mergeSeed(existing, "rev", "NEVER force-push"); !changed || restored {
				t.Fatalf("mergeSeed(%q): changed=%v restored=%v, want a change and no restoration", existing, changed, restored)
			}
		}
	})

	t.Run("mergeRules", func(t *testing.T) {
		for _, existing := range noBlock {
			if _, changed, restored := mergeRules(existing, "## sec\n\nNEVER force-push\n"); !changed || restored {
				t.Fatalf("mergeRules(%q): changed=%v restored=%v, want a change and no restoration", existing, changed, restored)
			}
		}
	})

	t.Run("a first ReconcileSeeds run is silent", func(t *testing.T) {
		cd := t.TempDir()
		res, err := ReconcileSeeds(cd, nil, []Artifact{seedArt("rev", "user", "NEVER force-push")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Written != 1 {
			t.Fatalf("fixture broken: res = %+v, want the seed written", res)
		}
		if len(res.Warnings) != 0 || len(res.Failures) != 0 {
			t.Fatalf("a first sync must say nothing: warnings=%v failures=%v", res.Warnings, res.Failures)
		}
	})

	t.Run("a first ReconcileRules run is silent", func(t *testing.T) {
		claudeDir := globalOnlyHome(t)
		proj := t.TempDir()
		rules := []Artifact{{ID: "r-1", Revision: 2, Type: "rule", Name: "sec", Content: "NEVER force-push"}}
		res, err := ReconcileRules(claudeDir, projs(proj), rules, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Written != 1 {
			t.Fatalf("fixture broken: res = %+v, want the project written", res)
		}
		if len(res.Warnings) != 0 || len(res.Failures) != 0 {
			t.Fatalf("a first sync must say nothing: warnings=%v failures=%v", res.Warnings, res.Failures)
		}
	})
}

// TestAFailedWriteClaimsNoRestoration gates the sentence both reconcilers carry
// over their restoration notice: it is reported AFTER the write, so a run that
// could not write claims nothing. Without this, hoisting the notice above the
// write is green, and the client tells a developer her edit was overwritten by
// content that never reached the disk.
func TestAFailedWriteClaimsNoRestoration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}

	t.Run("seed", func(t *testing.T) {
		cd := t.TempDir()
		arts := []Artifact{seedArt("rev", "user", "NEVER force-push")}
		if _, err := ReconcileSeeds(cd, nil, arts, nil); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(cd, "agent-memory", "rev")
		target := filepath.Join(dir, "MEMORY.md")
		must(t, os.WriteFile(target, []byte(tamperSeedBody(t, readFileT(t, target), "rev", "force-push is fine")), 0o644))
		// Readable, not writable: the read branch still passes and
		// writeFileAtomic's CreateTemp is what fails.
		must(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

		res, err := ReconcileSeeds(cd, nil, arts, nil)
		if err != nil {
			t.Fatalf("a per-target write failure must be non-fatal: %v", err)
		}
		if len(res.Failures) != 1 || res.Written != 0 {
			t.Fatalf("fixture broken: want exactly one write failure and nothing written, got %+v", res)
		}
		if hasRestorationNotice(res.Warnings) {
			t.Fatalf("a failed write claimed a restoration: %v", res.Warnings)
		}
	})

	t.Run("rules", func(t *testing.T) {
		claudeDir := globalOnlyHome(t)
		proj := t.TempDir()
		rules := []Artifact{{ID: "r-1", Revision: 2, Type: "rule", Name: "sec", Content: "NEVER force-push"}}
		if _, err := ReconcileRules(claudeDir, projs(proj), rules, nil); err != nil {
			t.Fatal(err)
		}
		agents := filepath.Join(proj, "AGENTS.md")
		must(t, os.WriteFile(agents, []byte(tamperRulesBody(t, readFileT(t, agents), "## sec\n\nforce-push is fine")), 0o644))
		must(t, os.Chmod(proj, 0o500))
		t.Cleanup(func() { _ = os.Chmod(proj, 0o755) })

		res, err := ReconcileRules(claudeDir, projs(proj), rules, nil)
		if err != nil {
			t.Fatalf("a per-project write failure must be non-fatal: %v", err)
		}
		if len(res.Failures) != 1 || res.Written != 0 {
			t.Fatalf("fixture broken: want exactly one write failure and nothing written, got %+v", res)
		}
		if hasRestorationNotice(res.Warnings) {
			t.Fatalf("a failed write claimed a restoration: %v", res.Warnings)
		}
	})
}
