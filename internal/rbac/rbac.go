// Package rbac contains pure authorization decision logic over entitlements.
// It has no database or transport dependencies and is exhaustively unit-tested.
package rbac

import "github.com/stefanocalabrese/orbeat-community/internal/store"

// VisibleServerIDs returns the set of mcp_server IDs granted by ents.
func VisibleServerIDs(ents []store.Entitlement) map[string]struct{} {
	out := make(map[string]struct{}, len(ents))
	for _, e := range ents {
		out[e.MCPServerID] = struct{}{}
	}
	return out
}

// AuthorizingEntitlement returns the entitlement in ents that grants tool on
// serverID -- the same entry ToolAllowed's own matching rule would stop on
// -- so a caller that needs to know not just THAT a call is allowed but
// WHICH entitlement (and therefore which role, via its RoleID) authorized it
// can read it off ok's result. This exists for usage-metering attribution
// (docs/specs/2026-08-25-orbeat-usage-metering-design.md section 2: "One
// call, one authorizing role, no double counting for a subject holding
// several") -- alongside ToolAllowed, not replacing it: a caller that only
// needs the bool keeps calling that one.
//
// ToolAllowed is defined IN TERMS OF this function, not the other way
// around, so the two can never disagree about which call is allowed or which
// entitlement decided it. Matching rule is unchanged: most-permissive wins
// across entitlements for the same server, first match in ents order.
func AuthorizingEntitlement(ents []store.Entitlement, serverID, tool string) (store.Entitlement, bool) {
	for _, e := range ents {
		if e.MCPServerID != serverID {
			continue
		}
		if e.AllowedTools == nil {
			return e, true
		}
		for _, t := range e.AllowedTools {
			if t == tool {
				return e, true
			}
		}
	}
	return store.Entitlement{}, false
}

// ToolAllowed reports whether ents permit calling tool on serverID.
// An entitlement whose AllowedTools is nil grants every tool on that server;
// a non-nil but empty AllowedTools denies all tools on that server.
// When multiple entitlements apply to the same server their grants are unioned
// (most-permissive wins): a nil-AllowedTools entry grants all tools even if
// another entry for the same server has a restricted or empty allowlist.
func ToolAllowed(ents []store.Entitlement, serverID, tool string) bool {
	_, ok := AuthorizingEntitlement(ents, serverID, tool)
	return ok
}
