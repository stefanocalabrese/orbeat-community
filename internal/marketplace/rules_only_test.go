package marketplace

import (
	"strings"
	"testing"
)

// TestRenderMarketplaceRulesOnlyPublishesNoArtifactsPlugin closes audit C8.
//
// The marketplace manifest decided whether to advertise the orbeat-artifacts
// plugin from len(artifacts) > 0, which is the COUNT OF INPUTS rather than the
// count of things rendered. `rule` is a valid artifact type with no Channel-1
// representation by design: rules reach developers through orbeat-sync's
// AGENTS.md block, never as plugin files. So a tenant whose only org artifacts
// are rules advertised a plugin whose entire content is its own manifest, and
// installing it produced nothing and no error, on the one surface whose job is
// to say what is available.
//
// TestRenderMarketplaceNoArtifactsPlugin covers the zero-artifacts case and
// passes on the defect, because there the input count is 0 too. Only a
// non-empty input that renders nothing tells the two predicates apart, which
// is why this case needed its own test rather than an extra assertion there.
func TestRenderMarketplaceRulesOnlyPublishesNoArtifactsPlugin(t *testing.T) {
	files, err := RenderMarketplace(
		Options{GatewayURL: "http://localhost:8090"},
		[]Artifact{
			{Name: "coding-standards", Type: "rule", Content: "use tabs"},
			{Name: "review-policy", Type: "rule", Content: "two approvals"},
		},
	)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	mp := files[".claude-plugin/marketplace.json"]
	if mp == "" {
		t.Fatal("no marketplace.json rendered, so this gate proved nothing")
	}
	if strings.Contains(mp, ArtifactsPluginName) {
		t.Errorf(`marketplace.json advertises %q for a tenant whose only artifacts are rules.

Rules render no plugin files, so the advertised plugin contains nothing but its own
manifest and installing it is a silent no-op. The manifest entry must follow what was
RENDERED, not how many artifacts were passed in (audit C8).

marketplace.json:
%s`, ArtifactsPluginName, mp)
	}
}

// TestRenderMarketplaceMixedStillPublishesTheArtifactsPlugin is the
// non-vacuity half: the fix must not swing the other way and suppress the
// plugin whenever a rule is present alongside real content.
func TestRenderMarketplaceMixedStillPublishesTheArtifactsPlugin(t *testing.T) {
	files, err := RenderMarketplace(
		Options{GatewayURL: "http://localhost:8090"},
		[]Artifact{
			{Name: "coding-standards", Type: "rule", Content: "use tabs"},
			{Name: "deploy-helper", Type: "skill", Content: "# Deploy"},
		},
	)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(files[".claude-plugin/marketplace.json"], ArtifactsPluginName) {
		t.Errorf("a rule alongside a real skill suppressed the artifacts plugin entry; "+
			"the skill renders content and must still be advertised: %s",
			files[".claude-plugin/marketplace.json"])
	}
}
