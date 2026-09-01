package api

import (
	"reflect"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// THIS FILE IS AN ORDINARY _test.go: syncListAuditCounts.add is pure math
// with no edition dependence (store.MaxGrantNames is shared, untagged code),
// so it belongs where internal/communitygen's generated Community tree still
// runs it, unlike sync_audit.ee_test.go's handler-level gates, which need
// s.pinning true to observe anything.
//
// These tests close open-points.md's "sync.list audit-count mutant" row:
// handleSyncArtifacts's counting used to be computed inline, from override
// (resolveSyncPayload's corrected verdict), but nothing could prove a mutant
// reading res[i].Reason instead would be caught, because the two values
// disagree only under the payload-race fallback, which has no
// single-threaded reproduction through the live handler. syncListAuditCounts
// (sync.go) is the extraction: add takes reason AND override as separate
// arguments, so a test can build a fixture where they disagree BY HAND and
// prove which one drives the count, no interface seam, mock, or timing
// dependence needed.
//
// TWO SEPARATE GATES, NOT ONE, AFTER REVIEW. The tests below only ever
// covered add's INSIDE: given a reason/override pair, does add read the
// right one. Review found that gap did not cover the CALL SITE inside
// handleSyncArtifacts, where a mutant substituting res[i].Reason for override
// compiled and passed clean, since both were plain strings and interchangeable
// to the compiler. That call site is now closed by a type distinction,
// correctedOverride (sync.go), not by a test in this file --
// TestCorrectedOverrideIsADistinctTypeFromString below pins the property that
// protection depends on, and the tests above it still cover add's own logic
// given a correct call site.

// TestSyncListAuditCountUsesTheCorrectedOverride is add's own mutant gate:
// given a reason/override pair, add must read override. reason is ""
// (what an honoured-or-unpinned pin's res[i].Reason would read) and override
// is pinOverridePruned (what resolveSyncPayload's payload-race fallback
// actually returns for the SAME artifact) -- the two computations this row
// names, made to disagree deliberately, the exact race shape open-points.md
// records as otherwise unreachable through a live handler. It does NOT cover
// whether handleSyncArtifacts hands add the right one; that is
// correctedOverride's job, see the file doc comment above.
//
// A version of add reading reason instead of override reports overridden=0
// here (since reason == ""); this test wants 1, which is what override
// demands. Measured red-proof: replacing `override != ""` with
// `reason != ""` in syncListAuditCounts.add makes this fail with
// "overridden = 0, want 1".
func TestSyncListAuditCountUsesTheCorrectedOverride(t *testing.T) {
	var c syncListAuditCounts
	c.add(false, "", pinOverridePruned, "race-fallback-artifact")

	if c.overridden != 1 {
		t.Errorf("overridden = %d, want 1: add must count from override (%q), never from reason (%q)",
			c.overridden, pinOverridePruned, "")
	}
	if len(c.overriddenArtifacts) != 1 || c.overriddenArtifacts[0] != "race-fallback-artifact" {
		t.Errorf("overriddenArtifacts = %v, want [race-fallback-artifact]", c.overriddenArtifacts)
	}
	if c.truncated {
		t.Errorf("truncated = true, want false: one entry never trips the cap")
	}
}

// TestSyncListAuditCountAgreeingFixtureWouldBeVacuous documents the failure
// mode the gate above exists to avoid: a fixture where reason and override
// happen to be equal cannot distinguish "counts from override" from "counts
// from reason" at all, so BOTH the correct code and the res[i].Reason mutant
// pass it. This test is not itself a gate on the mutant; it is proof the
// disagreeing fixture above is doing real work, by exercising the same call
// with reason and override set EQUAL and confirming the assertion technique
// (checking overridden) cannot tell the two implementations apart on this
// input -- overridden is 1 whether add reads reason or override, since both
// are pinOverrideFloor here.
func TestSyncListAuditCountAgreeingFixtureWouldBeVacuous(t *testing.T) {
	var c syncListAuditCounts
	c.add(false, pinOverrideFloor, pinOverrideFloor, "agreeing-artifact")

	if c.overridden != 1 {
		t.Fatalf("overridden = %d, want 1 (sanity: this fixture must still register an override)", c.overridden)
	}
	// No assertion beyond this distinguishes reason-driven from
	// override-driven counting on an agreeing fixture -- which is exactly
	// why TestSyncListAuditCountUsesTheCorrectedOverride above sets them
	// apart instead.
}

// TestSyncListAuditCountDoesNotDoubleCountOverriddenIntoPinned red-proves the
// off-by-one an earlier review caught at the handler level (see
// TestSyncListAuditReportsPinnedAndOverriddenExactly's doc comment in
// sync_audit.ee_test.go): an artifact that is both pinned (hasPin=true) and
// overridden (override != "") must increment pinned exactly once and
// overridden exactly once, never overridden folded into pinned a second
// time.
//
// Measured red-proof: adding a second `c.pinned++` inside the
// `if override != ""` branch of syncListAuditCounts.add makes this fail with
// "pinned = 2, want 1".
func TestSyncListAuditCountDoesNotDoubleCountOverriddenIntoPinned(t *testing.T) {
	var c syncListAuditCounts
	c.add(true, pinOverrideFloor, pinOverrideFloor, "pinned-and-overridden")

	if c.pinned != 1 {
		t.Errorf("pinned = %d, want 1: an overridden pin must not be counted into pinned a second time", c.pinned)
	}
	if c.overridden != 1 {
		t.Errorf("overridden = %d, want 1", c.overridden)
	}
}

// TestSyncListAuditCountCapsNamesButNeverTheCount is the asymmetry the task
// names as most likely to be broken by a careless extraction:
// overriddenArtifacts caps at store.MaxGrantNames, but overridden itself
// stays an exact count regardless, and truncated says only whether the name
// list stopped growing.
//
// The fixture size is derived from store.MaxGrantNames rather than a literal
// 51, so raising the cap cannot leave this test asserting a number the code
// no longer uses (mirrors
// TestSyncListAuditCapsOverriddenNamesButNeverTheCount's own reasoning in
// sync_audit.ee_test.go).
func TestSyncListAuditCountCapsNamesButNeverTheCount(t *testing.T) {
	const n = store.MaxGrantNames + 1
	var c syncListAuditCounts
	for range n {
		c.add(false, pinOverrideAhead, pinOverrideAhead, "art")
	}

	if c.overridden != n {
		t.Errorf("overridden = %d, want %d: the COUNT must stay exact even once the name list stops growing", c.overridden, n)
	}
	if len(c.overriddenArtifacts) != store.MaxGrantNames {
		t.Errorf("len(overriddenArtifacts) = %d, want %d (the cap)", len(c.overriddenArtifacts), store.MaxGrantNames)
	}
	if !c.truncated {
		t.Error("truncated = false, want true: the cap bit on the (n+1)th entry")
	}
	if c.pinned != 0 {
		t.Errorf("pinned = %d, want 0: hasPin was false on every call", c.pinned)
	}
}

// TestSyncListAuditCountCapAtExactlyTheLimitStaysUntruncated is the boundary
// complement to the test above: exactly store.MaxGrantNames overridden
// entries must fill the name list WITHOUT tripping truncated, so a
// off-by-one in the cap comparison (< vs <=) is caught in either direction.
func TestSyncListAuditCountCapAtExactlyTheLimitStaysUntruncated(t *testing.T) {
	var c syncListAuditCounts
	for range store.MaxGrantNames {
		c.add(false, pinOverrideFloor, pinOverrideFloor, "art")
	}

	if c.overridden != store.MaxGrantNames {
		t.Errorf("overridden = %d, want %d", c.overridden, store.MaxGrantNames)
	}
	if len(c.overriddenArtifacts) != store.MaxGrantNames {
		t.Errorf("len(overriddenArtifacts) = %d, want %d", len(c.overriddenArtifacts), store.MaxGrantNames)
	}
	if c.truncated {
		t.Error("truncated = true, want false: exactly the cap must not trip it")
	}
}

// TestCorrectedOverrideIsADistinctTypeFromString pins the property the
// call-site protection above depends on: correctedOverride (sync.go) must be
// a genuinely distinct DEFINED type from string (`type correctedOverride
// string`), never a type ALIAS (`type correctedOverride = string`). An alias
// is indistinguishable from string at every call site, so a call passing
// res[i].Reason where a correctedOverride is expected would compile again
// with no error at all, silently reopening the exact defect this row exists
// to close -- reading the source is not enough to see the difference, an
// alias and a defined type look identical everywhere except this property,
// which is why it needs its own test rather than a comment.
//
// This cannot be red-proven by mutating sync.go the way the other gates in
// this file are: `type correctedOverride = string` and `type
// correctedOverride string` both compile, both pass every other test in this
// file (the two-string case doesn't even get less type-safe FOR THOSE TESTS,
// since every value they pass is an untyped constant that adapts to either
// form), and the whole package still builds either way. Only this test, or
// the compiler error the type distinction is supposed to produce on a real
// call-site mutant, can tell the two apart.
func TestCorrectedOverrideIsADistinctTypeFromString(t *testing.T) {
	var o correctedOverride
	var s string
	to, ts := reflect.TypeOf(o), reflect.TypeOf(s)

	if to == ts {
		t.Fatal("correctedOverride and string compare equal under reflect.TypeOf, which only happens if correctedOverride is a type ALIAS for string rather than a distinct defined type -- the call-site protection every other test in this file assumes is gone")
	}
	if to.Kind() != reflect.String {
		t.Fatalf("correctedOverride's Kind() = %v, want String: it must still behave like a string everywhere else", to.Kind())
	}
}
