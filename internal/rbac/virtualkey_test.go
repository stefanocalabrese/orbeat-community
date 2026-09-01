package rbac

import (
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// TestKeyNarrowingCanOnlyRemove is the load-bearing test of the whole feature.
// The role union is most-permissive-wins (rbac.go:17-20), so a narrowing
// written as another entitlement would WIDEN access. This proves it cannot.
func TestKeyNarrowingCanOnlyRemove(t *testing.T) {
	ents := []store.Entitlement{
		{MCPServerID: "srv1", AllowedTools: []string{"read"}},
	}
	cases := []struct {
		name   string
		narrow []string
		tool   string
		want   bool
	}{
		{"nil narrowing keeps the role's grant", nil, "read", true},
		{"nil narrowing grants nothing extra", nil, "write", false},
		{"narrowing to the granted tool keeps it", []string{"read"}, "read", true},
		{"narrowing to another tool removes it", []string{"write"}, "read", false},
		{"NARROWING CANNOT GRANT what the role lacks", []string{"write"}, "write", false},
		{"empty narrowing denies everything", []string{}, "read", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := KeyToolAllowed(ents, c.narrow, "srv1", c.tool)
			if got != c.want {
				t.Errorf("KeyToolAllowed(narrow=%v, tool=%q) = %v, want %v", c.narrow, c.tool, got, c.want)
			}
		})
	}
}

// TestKeyNarrowingIsScopedByCallerNotByThisFunction pins the contract that
// caught a false claim in the spec: entitlement.allowed_tools sits ON one
// server-scoped row, but a virtual key's stored allowed_tools is a flat,
// namespaced list spanning every server the role can see. If that flat list
// were passed to KeyToolAllowed unfiltered, a narrowing meant for one server
// would silently also apply to any other server granting the same bare tool
// name: measured with this exact fixture, narrow=["read"] passed unfiltered
// allowed BOTH srv1/read and srv2/read, though the key was only ever meant
// to narrow srv1.
//
// KeyToolAllowed does not resolve namespacing itself; the caller MUST pass
// narrow already filtered to serverID. This test proves that under that
// contract the cross-server leak is unexpressible: a key that says nothing
// about srv2 arrives at this function as an EMPTY (non-nil) slice when
// checking srv2, which denies srv2/read even though the role grants it and
// even though "read" appears in the key's flat list for srv1.
func TestKeyNarrowingIsScopedByCallerNotByThisFunction(t *testing.T) {
	// The role grants the SAME bare tool name on two different servers: the
	// exact shape that made an unfiltered flat list dangerous.
	ents := []store.Entitlement{
		{MCPServerID: "srv1", AllowedTools: []string{"read"}},
		{MCPServerID: "srv2", AllowedTools: []string{"read"}},
	}

	// The key narrows to "read" on srv1 only. A correct caller resolves
	// slug__tool and passes narrow filtered to serverID: checking srv1 gets
	// ["read"], checking srv2 gets an empty slice because none of the key's
	// narrowing entries belong to srv2.
	narrowFilteredForSrv1 := []string{"read"}
	narrowFilteredForSrv2 := []string{}

	if !KeyToolAllowed(ents, narrowFilteredForSrv1, "srv1", "read") {
		t.Error("srv1/read should be allowed: the role grants it and the srv1-filtered narrowing names it")
	}
	if KeyToolAllowed(ents, narrowFilteredForSrv2, "srv2", "read") {
		t.Error("srv2/read must be denied even though the role grants it: the key's narrowing says " +
			"nothing about srv2, so a correct caller filters to an empty list there. Allowing it here " +
			"would mean the narrowing crossed a server boundary because the two servers happen to " +
			"share a bare tool name.")
	}
}
