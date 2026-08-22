package gateway

import "testing"

func TestNamespaceAndSplit(t *testing.T) {
	full := Namespace("github", "create_issue")
	if full != "github__create_issue" {
		t.Fatalf("Namespace = %q", full)
	}
	slug, tool, ok := Split("github__create_issue")
	if !ok || slug != "github" || tool != "create_issue" {
		t.Fatalf("Split = %q,%q,%v", slug, tool, ok)
	}
	// Tool names may themselves contain underscores; split on the FIRST "__".
	slug, tool, ok = Split("github__sub__tool")
	if !ok || slug != "github" || tool != "sub__tool" {
		t.Fatalf("Split nested = %q,%q,%v", slug, tool, ok)
	}
	if _, _, ok := Split("no-separator"); ok {
		t.Fatal("Split should fail without a __ separator")
	}
}

func TestNamespaceSplitRoundTrip(t *testing.T) {
	cases := []struct{ slug, tool string }{
		{"github", "create_issue"},
		{"a-b", "sub__tool"},
		{"x", "t"},
	}
	for _, c := range cases {
		s, tl, ok := Split(Namespace(c.slug, c.tool))
		if !ok || s != c.slug || tl != c.tool {
			t.Fatalf("round-trip(%q,%q) = %q,%q,%v", c.slug, c.tool, s, tl, ok)
		}
	}
}
