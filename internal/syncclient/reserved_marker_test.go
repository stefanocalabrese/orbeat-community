package syncclient

import (
	"strings"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/govern"
)

// TestGovernReservedMarkerCoversEverySyncSentinel is the cross-package gate
// behind audit finding A15's write-time reject. internal/api's
// validateArtifact refuses artifact content and memory seeds that carry a
// managed-block sentinel, and it asks govern.HasReservedMarker what a
// sentinel is. That reject is worth exactly as much as the overlap between
// govern's ONE regex and the literals THIS package actually looks for on
// disk, and the two live in packages that never import each other, so
// nothing but this test can compare them.
//
// The comparison runs against the real writers and the real matchers, never
// against hand-typed marker strings: renderRulesBlock and renderSeedBlock
// produce the bytes, and rulesBeginRe/rulesEndRe/seedBeginRe/seedEndRe are
// the patterns whose counts decide rulesMarkersHealthy and
// seedMarkersHealthy. A test spelling the markers out itself would keep
// passing after either side's format changed, which is the whole failure
// shape A15 is an instance of.
//
// Direction matters and only one direction is asserted: every sentinel this
// package writes or hunts for must be caught by govern. The converse is
// deliberately not required, because govern's pattern is broader on purpose
// (it accepts "<!--ORBEAT-RULES:END" with no space, which no writer here
// emits) and demanding equality would fail on a superset that is strictly
// safer.
func TestGovernReservedMarkerCoversEverySyncSentinel(t *testing.T) {
	// The bytes the two writers actually put on a developer's disk.
	written := map[string]string{
		"renderRulesBlock": renderRulesBlock("body text"),
		"renderSeedBlock":  renderSeedBlock("reviewer", "body text"),
	}
	for name, block := range written {
		if !govern.HasReservedMarker(block) {
			t.Errorf("govern.HasReservedMarker does not match %s's output, so artifact content "+
				"could forge one and validateArtifact would accept it:\n%s", name, block)
		}
	}

	// The markers whose COUNTS decide whether a file is spliceable. An
	// unmatched one here is the A15 failure exactly: the block goes out
	// through the catalog, the client counts an extra END, and the project is
	// skipped for good.
	sample := renderRulesBlock("body text") + renderSeedBlock("reviewer", "body text")
	markers := map[string]string{
		"rulesBeginRe": rulesBeginRe.FindString(sample),
		"rulesEndRe":   rulesEndRe.FindString(sample),
		"seedBeginRe":  seedBeginRe("reviewer").FindString(sample),
		"seedEndRe":    seedEndRe("reviewer").FindString(sample),
	}
	for name, lit := range markers {
		if lit == "" {
			t.Fatalf("%s matched nothing in the rendered blocks: this test's own premise is broken, "+
				"not the coverage it is about to check", name)
		}
		if !govern.HasReservedMarker(lit) {
			t.Errorf("%s counts the literal %q, which govern.HasReservedMarker does NOT match: "+
				"an artifact carrying it is accepted at write time and corrupts the file it lands in", name, lit)
		}
	}
}

// TestGovernReservedMarkerLeavesProseAlone pins the other half, and it is a
// real product decision rather than an incidental gap: an artifact that only
// NAMES the feature must stay publishable. A rule artifact documenting how
// orbeat's managed blocks work is the most likely rule anyone writes, and a
// bare-substring reject would make writing one impossible. The corrupting
// form is the HTML comment, because that is the only form
// rulesMarkersHealthy and seedMarkersHealthy count.
func TestGovernReservedMarkerLeavesProseAlone(t *testing.T) {
	prose := "orbeat-sync owns one ORBEAT-RULES block per project and one ORBEAT-SEED block per subagent."
	if govern.HasReservedMarker(prose) {
		t.Fatalf("prose naming the sentinels must stay publishable, got a match on %q", prose)
	}
	if strings.Count(prose, "ORBEAT-") != 2 {
		t.Fatal("the fixture no longer mentions both sentinel names, so it proves nothing")
	}
}
