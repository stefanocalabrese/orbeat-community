package govern

import (
	"encoding/json"
	"testing"
)

func TestDigestEmptySet(t *testing.T) {
	if got := Digest(nil); got != "" {
		t.Fatalf("Digest(nil) = %q, want \"\" (the no-findings sentinel)", got)
	}
	if got := Digest([]Finding{}); got != "" {
		t.Fatalf("Digest(empty slice) = %q, want \"\" (the no-findings sentinel)", got)
	}
}

func TestDigestEqualSetsAgree(t *testing.T) {
	a := []Finding{
		{Rule: "secret", Message: "possible AWS access key ID in content", Severity: "block"},
		{Rule: "size", Message: "content exceeds 64KiB", Severity: "warn"},
	}
	b := []Finding{
		{Rule: "secret", Message: "possible AWS access key ID in content", Severity: "block"},
		{Rule: "size", Message: "content exceeds 64KiB", Severity: "warn"},
	}
	da, db := Digest(a), Digest(b)
	if da != db {
		t.Fatalf("equal finding sets produced different digests: %q vs %q", da, db)
	}
	if da == "" {
		t.Fatal("a non-empty finding set must not digest to the empty-set sentinel")
	}
}

// TestDigestChangesPerField asserts field-by-field, not with one combined
// case: a test that only checks "these two whole sets differ" can pass while
// the digest silently ignores one of the three fields, as long as the OTHER
// two fields still differ between the cases. Each sub-test holds two fields
// fixed and edits exactly one.
func TestDigestChangesPerField(t *testing.T) {
	base := Finding{Rule: "secret", Message: "possible AWS access key ID in content", Severity: "block"}
	baseDigest := Digest([]Finding{base})

	t.Run("rule", func(t *testing.T) {
		f := base
		f.Rule = "other-rule"
		if got := Digest([]Finding{f}); got == baseDigest {
			t.Fatalf("changing Rule alone did not change the digest (%q)", got)
		}
	})
	t.Run("message", func(t *testing.T) {
		f := base
		f.Message = "a completely different message"
		if got := Digest([]Finding{f}); got == baseDigest {
			t.Fatalf("changing Message alone did not change the digest (%q)", got)
		}
	})
	t.Run("severity", func(t *testing.T) {
		f := base
		f.Severity = "warn"
		if got := Digest([]Finding{f}); got == baseDigest {
			t.Fatalf("changing Severity alone did not change the digest (%q)", got)
		}
	})
}

// TestDigestOrderInsensitive pins the deliberate design decision (see the
// Digest doc comment): the LLM scanner's finding order is not guaranteed
// stable between runs, so two orderings of the identical set must agree, or a
// re-scan that found exactly the same findings in a different order would
// spuriously invalidate a still-valid acknowledgment.
func TestDigestOrderInsensitive(t *testing.T) {
	f1 := Finding{Rule: "secret", Message: "m1", Severity: "block"}
	f2 := Finding{Rule: "size", Message: "m2", Severity: "warn"}
	f3 := Finding{Rule: "llm-flagged", Message: "m3", Severity: "info"}

	forward := Digest([]Finding{f1, f2, f3})
	reversed := Digest([]Finding{f3, f2, f1})
	shuffled := Digest([]Finding{f2, f3, f1})

	if forward != reversed || forward != shuffled {
		t.Fatalf("digest is order-sensitive: forward=%q reversed=%q shuffled=%q", forward, reversed, shuffled)
	}
}

// TestDigestDuplicatesDoNotCollapse: the LLM layer can genuinely emit the
// same {rule,message,severity} finding more than once in one scan (see
// parseLLMFindings in llm_scanner.ee.go, which does not dedupe its output,
// and CompositeScanner, which just concatenates every scanner's findings).
// Two occurrences of an identical finding must not digest the same as one.
func TestDigestDuplicatesDoNotCollapse(t *testing.T) {
	f := Finding{Rule: "llm-flagged", Message: "looks risky", Severity: "warn"}
	one := Digest([]Finding{f})
	two := Digest([]Finding{f, f})
	three := Digest([]Finding{f, f, f})

	if one == two {
		t.Fatalf("two occurrences of the same finding digested the same as one occurrence (%q)", one)
	}
	if two == three {
		t.Fatalf("three occurrences of the same finding digested the same as two occurrences (%q)", two)
	}
}

// TestDigestStableAcrossJSONRoundTrip: findings are stored as jsonb and read
// back into a fresh []Finding before a later digest comparison (the
// acknowledgment endpoint re-derives the current digest from the stored
// findings). The digest of the decoded slice must equal the digest computed
// before it was ever marshalled.
func TestDigestStableAcrossJSONRoundTrip(t *testing.T) {
	findings := []Finding{
		{Rule: "secret", Message: "possible AWS access key ID in content", Severity: "block"},
		{Rule: "size", Message: "content exceeds 64KiB", Severity: "warn"},
		{Rule: "llm-flagged", Message: "looks risky", Severity: "warn"},
	}
	before := Digest(findings)

	raw, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	var after []Finding
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}

	if got := Digest(after); got != before {
		t.Fatalf("digest changed across a JSON marshal/unmarshal round trip: before=%q after=%q", before, got)
	}
}

// TestDigestFormat pins requirement 5: short, URL-safe, case-stable. Hex
// digits carry the same value whether upper or lower case (unlike base64,
// where case is significant to the decoded value), which is what makes the
// format stable if something along the way case-folds it.
func TestDigestFormat(t *testing.T) {
	d := Digest([]Finding{{Rule: "secret", Message: "m", Severity: "block"}})
	if len(d) != 64 {
		t.Fatalf("digest length = %d, want 64 (a sha256 sum, hex-encoded)", len(d))
	}
	for _, r := range d {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Fatalf("digest %q contains a non-lowercase-hex character %q", d, r)
		}
	}
}
