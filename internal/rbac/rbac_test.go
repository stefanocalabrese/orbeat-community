package rbac

import (
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

func TestVisibleServerIDs(t *testing.T) {
	ents := []store.Entitlement{
		{MCPServerID: "s1"}, {MCPServerID: "s2"}, {MCPServerID: "s1"},
	}
	got := VisibleServerIDs(ents)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if _, ok := got["s1"]; !ok {
		t.Fatal("s1 missing")
	}
	if _, ok := got["s2"]; !ok {
		t.Fatal("s2 missing")
	}
}

func TestToolAllowed(t *testing.T) {
	ents := []store.Entitlement{
		{MCPServerID: "s1", AllowedTools: []string{"read", "list"}},
		{MCPServerID: "s2", AllowedTools: nil},        // nil = all tools
		{MCPServerID: "s4", AllowedTools: []string{}}, // empty non-nil => deny all
	}
	cases := []struct {
		server, tool string
		want         bool
	}{
		{"s1", "read", true},
		{"s1", "delete", false},  // not in allowlist
		{"s2", "anything", true}, // nil => all
		{"s3", "read", false},    // no entitlement
		{"s4", "read", false},    // empty non-nil => deny all
		{"s4", "", false},        // empty non-nil => deny all (empty tool name)
	}
	for _, c := range cases {
		if got := ToolAllowed(ents, c.server, c.tool); got != c.want {
			t.Errorf("ToolAllowed(%q,%q) = %v, want %v", c.server, c.tool, got, c.want)
		}
	}
}

// TestToolAllowedUnionsAcrossEntitlements verifies that grants from multiple
// entitlements on the same server are unioned (most-permissive wins).
// A nil-AllowedTools entry grants all tools, even if another entry for the
// same server has a restricted allowlist.
func TestToolAllowedUnionsAcrossEntitlements(t *testing.T) {
	ents := []store.Entitlement{
		{MCPServerID: "s", AllowedTools: []string{"read"}}, // restricted
		{MCPServerID: "s", AllowedTools: nil},              // nil = all tools
	}
	// Union of restricted + nil => all tools allowed.
	if !ToolAllowed(ents, "s", "delete") {
		t.Error("ToolAllowed(s, delete) = false; want true (nil entitlement grants all)")
	}
	if !ToolAllowed(ents, "s", "read") {
		t.Error("ToolAllowed(s, read) = false; want true")
	}
}

func TestEmptyEntitlements(t *testing.T) {
	if len(VisibleServerIDs(nil)) != 0 {
		t.Fatal("expected no visible servers")
	}
	if ToolAllowed(nil, "s1", "read") {
		t.Fatal("expected deny with no entitlements")
	}
}
