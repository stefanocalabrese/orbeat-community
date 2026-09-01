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

// TestAuthorizingEntitlementAttributesToTheGrantingRole is Task 1's
// correction gate: a subject holding TWO roles, where only ONE of them
// grants the tool being called, must attribute the call to the GRANTING
// role -- never the session's first role regardless of which one actually
// authorized it (docs/specs/2026-08-25-orbeat-usage-metering-design.md
// section 2). A fixture where the subject holds only one role cannot tell
// "the correct role" apart from "whichever role happened to be present", so
// both orderings are built deliberately: the granting role listed second
// catches a "always return ents[0]'s role" mutant, and the granting role
// listed first catches the opposite "always return the last match" mutant.
func TestAuthorizingEntitlementAttributesToTheGrantingRole(t *testing.T) {
	cases := []struct {
		name       string
		ents       []store.Entitlement
		wantRoleID string
	}{
		{
			name: "granting role listed second",
			ents: []store.Entitlement{
				{RoleID: "role-viewer", MCPServerID: "s1", AllowedTools: []string{"list"}},  // does NOT grant "write"
				{RoleID: "role-editor", MCPServerID: "s1", AllowedTools: []string{"write"}}, // grants "write"
			},
			wantRoleID: "role-editor",
		},
		{
			name: "granting role listed first",
			ents: []store.Entitlement{
				{RoleID: "role-editor", MCPServerID: "s1", AllowedTools: []string{"write"}}, // grants "write"
				{RoleID: "role-viewer", MCPServerID: "s1", AllowedTools: []string{"list"}},  // does NOT grant "write"
			},
			wantRoleID: "role-editor",
		},
	}
	for _, c := range cases {
		got, ok := AuthorizingEntitlement(c.ents, "s1", "write")
		if !ok {
			t.Fatalf("%s: AuthorizingEntitlement = not ok, want ok (role-editor grants \"write\")", c.name)
		}
		if got.RoleID != c.wantRoleID {
			t.Errorf("%s: RoleID = %q, want %q -- a subject holding two roles, only one of which grants "+
				"the tool, must attribute to the GRANTING role, never the session's first role regardless "+
				"of which one actually authorized the call", c.name, got.RoleID, c.wantRoleID)
		}
	}

	// A call neither role grants must report not-ok, not a zero-value role
	// mistaken for an attribution.
	ents := []store.Entitlement{
		{RoleID: "role-viewer", MCPServerID: "s1", AllowedTools: []string{"list"}},
		{RoleID: "role-editor", MCPServerID: "s1", AllowedTools: []string{"write"}},
	}
	if _, ok := AuthorizingEntitlement(ents, "s1", "delete"); ok {
		t.Fatal("AuthorizingEntitlement(s1, delete) = ok, want not ok -- neither role grants \"delete\"")
	}
}
