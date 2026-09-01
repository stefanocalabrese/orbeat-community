package rbac

import "github.com/stefanocalabrese/orbeat-community/internal/store"

// KeyToolAllowed is a virtual key's authorization decision: the owning role's
// live grants INTERSECTED with the key's narrowing.
//
// THE NARROWING IS A SEPARATE PREDICATE, APPLIED AFTER ToolAllowed, AND NEVER
// AN ENTRY IN ents. ToolAllowed unions entitlements most-permissively
// (rbac.go:17-20), so a narrowing appended to ents would GRANT the tools it
// names rather than restrict to them, handing the robot access the role never
// had. That mistake reads as correct code. This signature makes it
// unexpressible: narrow is a distinct parameter of a distinct type position,
// and it can only ever remove.
//
// narrow is bare tool names, ALREADY FILTERED TO serverID BY THE CALLER. A
// virtual key's stored allowed_tools list is flat and namespaced at rest
// (e.g. "github__create_issue", spanning every server the role can see); an
// entitlement's allowed_tools sits ON one row and is server-scoped for free.
// Passing the key's flat list straight into this function would make the
// narrowing server-blind: a key narrowed to "read" would allow "read" on
// every server the role grants it on, not just the one the narrowing was
// meant for (measured with a two-server fixture: narrow=["read"] allowed
// srv1/read AND srv2/read). rbac stays free of namespacing on purpose, so
// the caller (the gateway, which already resolves slug__tool) must do the
// splitting and pass only the entries belonging to serverID before calling.
//
// Under that contract: nil narrow means "everything the role allows on
// serverID", matching entitlement.allowed_tools semantics (store/rbac.go:17).
// A non-nil list narrows to those bare names on serverID. An empty (non-nil)
// list denies all on serverID, including the case where the key narrows
// tools on OTHER servers but names none on this one; that is correct, not
// an edge case to special-case away, because it is what keeps the narrowing
// from crossing server boundaries.
func KeyToolAllowed(ents []store.Entitlement, narrow []string, serverID, tool string) bool {
	if !ToolAllowed(ents, serverID, tool) {
		return false
	}
	if narrow == nil {
		return true
	}
	for _, t := range narrow {
		if t == tool {
			return true
		}
	}
	return false
}
