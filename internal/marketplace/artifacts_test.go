package marketplace

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseFrontmatterRequiresNameAndDescription(t *testing.T) {
	if err := ValidateArtifactContent("---\nname: x\ndescription: y\n---\nbody"); err != nil {
		t.Errorf("valid content rejected: %v", err)
	}
	if err := ValidateArtifactContent("no frontmatter"); err == nil {
		t.Error("expected error for missing frontmatter")
	}
	if err := ValidateArtifactContent("---\nname: x\n---\nbody"); err == nil {
		t.Error("expected error for missing description")
	}
}

func TestRenderArtifactsPluginPathsAndMemory(t *testing.T) {
	files, err := RenderArtifactsPlugin([]Artifact{
		{Type: "skill", Name: "fmt", Content: "---\nname: fmt\ndescription: formats\n---\nrun gofmt"},
		{Type: "subagent", Name: "reviewer", Content: "---\nname: reviewer\ndescription: reviews\n---\nyou review", MemoryScope: "project"},
		{Type: "subagent", Name: "plain", Content: "---\nname: plain\ndescription: p\n---\nx"}, // no memory
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	skill := files["plugins/orbeat-artifacts/skills/fmt/SKILL.md"]
	if skill != "---\nname: fmt\ndescription: formats\n---\nrun gofmt" {
		t.Errorf("skill content not verbatim: %q", skill)
	}
	rev := files["plugins/orbeat-artifacts/agents/reviewer.md"]
	if !strings.Contains(rev, "memory: project") {
		t.Errorf("memory not injected: %q", rev)
	}
	want := "---\ndescription: reviews\nmemory: project\nname: reviewer\n---\nyou review"
	if rev != want {
		t.Errorf("reviewer.md exact render mismatch:\ngot:  %q\nwant: %q", rev, want)
	}
	plain := files["plugins/orbeat-artifacts/agents/plain.md"]
	if strings.Contains(plain, "memory:") {
		t.Errorf("memory should be absent: %q", plain)
	}
	if _, ok := files["plugins/orbeat-artifacts/.claude-plugin/plugin.json"]; !ok {
		t.Error("missing plugin.json")
	}
}

func TestRenderMarketplaceListsBothPlugins(t *testing.T) {
	files, err := RenderMarketplace(Options{GatewayURL: "http://localhost:8090"}, []Artifact{
		{Type: "skill", Name: "fmt", Content: "---\nname: fmt\ndescription: d\n---\nx"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mp := files[".claude-plugin/marketplace.json"]
	if !strings.Contains(mp, `"orbeat-gateway"`) || !strings.Contains(mp, `"orbeat-artifacts"`) {
		t.Errorf("marketplace.json must list both plugins: %s", mp)
	}
	// connect plugin still present
	if _, ok := files["plugins/orbeat-gateway/.mcp.json"]; !ok {
		t.Error("connect plugin missing from composed marketplace")
	}
}

func TestSplitFrontmatterRobustness(t *testing.T) {
	// a "---extra" key must not be mistaken for the closing fence
	if err := ValidateArtifactContent("---\nname: x\ndescription: d\n---extra: y\n---\nbody"); err != nil {
		t.Errorf("---extra key should parse, got: %v", err)
	}
	// CRLF content is normalized and accepted
	if err := ValidateArtifactContent("---\r\nname: x\r\ndescription: d\r\n---\r\nbody"); err != nil {
		t.Errorf("CRLF content should be accepted, got: %v", err)
	}
}

func TestRenderMarketplaceNoArtifactsPlugin(t *testing.T) {
	files, err := RenderMarketplace(Options{GatewayURL: "http://localhost:8090"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mp := files[".claude-plugin/marketplace.json"]
	if strings.Contains(mp, "orbeat-artifacts") {
		t.Errorf("empty artifacts must not add the artifacts plugin entry: %s", mp)
	}
}

// TestRenderArtifactsPluginRejectsInvalidSlugName pins audit G12: the renderer
// must hold the slug invariant independently of the API boundary. It cannot
// trust that every Artifact.Name reaching it was validated upstream — a
// direct DB write (migration, admin SQL fix, future API bug) could smuggle a
// path-traversal or absolute-path segment into a.Name, and this is the last
// line of defense before that name becomes part of an on-disk file path.
func TestRenderArtifactsPluginRejectsInvalidSlugName(t *testing.T) {
	cases := []struct {
		name  string
		aType string
	}{
		{"../../etc/passwd", "skill"},
		{"Uppercase", "subagent"},
		{"has space", "skill"},
		{"", "subagent"},
		{"-leading-dash", "skill"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := RenderArtifactsPlugin([]Artifact{
				{Type: c.aType, Name: c.name, Content: "---\nname: x\ndescription: d\n---\nbody"},
			})
			if err == nil {
				t.Fatalf("expected an error for invalid artifact name %q, got nil", c.name)
			}
		})
	}
}

// TestRenderMarketplacePropagatesInvalidSlugError verifies the composed
// RenderMarketplace surfaces the same guard (it must not silently drop the
// error while merging the artifacts-plugin file map into the full tree).
func TestRenderMarketplacePropagatesInvalidSlugError(t *testing.T) {
	_, err := RenderMarketplace(Options{GatewayURL: "http://localhost:8090"}, []Artifact{
		{Type: "skill", Name: "../evil", Content: "---\nname: x\ndescription: d\n---\nbody"},
	})
	if err == nil {
		t.Fatal("expected RenderMarketplace to reject an invalid artifact name, got nil")
	}
}

// TestRenderArtifactsPluginWarnsOnMemoryInjectionFailure pins the second half
// of audit G12: withMemory used to fail SILENTLY, falling back to verbatim
// content with no signal that the governed memory injection was dropped. A
// slog.Warn must fire so the drop is observable.
func TestRenderArtifactsPluginWarnsOnMemoryInjectionFailure(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	files, err := RenderArtifactsPlugin([]Artifact{
		{Type: "subagent", Name: "broken", Content: "no frontmatter at all", MemoryScope: "project"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := files["plugins/orbeat-artifacts/agents/broken.md"]
	if got != "no frontmatter at all" {
		t.Errorf("expected verbatim fallback content, got %q", got)
	}
	if !strings.Contains(buf.String(), "memory injection") {
		t.Errorf("expected a warn log for the dropped memory injection, got: %q", buf.String())
	}
}

// TestRenderArtifactContentWarnsOnMemoryInjectionFailure mirrors the above for
// the single-artifact Channel-2 path.
func TestRenderArtifactContentWarnsOnMemoryInjectionFailure(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	a := Artifact{Type: "subagent", Name: "broken", Content: "no frontmatter at all", MemoryScope: "project"}
	got := RenderArtifactContent(a)
	if got != "no frontmatter at all" {
		t.Errorf("expected verbatim fallback content, got %q", got)
	}
	if !strings.Contains(buf.String(), "memory injection") {
		t.Errorf("expected a warn log for the dropped memory injection, got: %q", buf.String())
	}
}

func TestRenderArtifactContent(t *testing.T) {
	// Skill: returned verbatim.
	sk := Artifact{Type: "skill", Name: "fmt", Content: "---\nname: fmt\ndescription: d\n---\nbody"}
	if got := RenderArtifactContent(sk); got != sk.Content {
		t.Fatalf("skill should be verbatim, got %q", got)
	}
	// Subagent with memory scope: `memory:` injected into frontmatter.
	sub := Artifact{Type: "subagent", Name: "rev", Content: "---\nname: rev\ndescription: d\n---\nbody", MemoryScope: "project"}
	if got := RenderArtifactContent(sub); !strings.Contains(got, "memory: project") {
		t.Fatalf("subagent memory not injected: %q", got)
	}
	// Subagent without scope: verbatim.
	sub2 := Artifact{Type: "subagent", Name: "rev2", Content: "---\nname: rev2\ndescription: d\n---\nbody"}
	if got := RenderArtifactContent(sub2); got != sub2.Content {
		t.Fatalf("no-scope subagent should be verbatim, got %q", got)
	}
}
