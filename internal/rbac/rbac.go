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

// ToolAllowed reports whether ents permit calling tool on serverID.
// An entitlement whose AllowedTools is nil grants every tool on that server;
// a non-nil but empty AllowedTools denies all tools on that server.
// When multiple entitlements apply to the same server their grants are unioned
// (most-permissive wins): a nil-AllowedTools entry grants all tools even if
// another entry for the same server has a restricted or empty allowlist.
func ToolAllowed(ents []store.Entitlement, serverID, tool string) bool {
	for _, e := range ents {
		if e.MCPServerID != serverID {
			continue
		}
		if e.AllowedTools == nil {
			return true
		}
		for _, t := range e.AllowedTools {
			if t == tool {
				return true
			}
		}
	}
	return false
}
