package api

import "context"

// isVirtualKeyClient is the Community edition's half of this extension point
// (see virtual_key_gate.ee.go's doc comment for the shared contract).
// Virtual keys are Enterprise-only (docs/specs/2026-08-25-orbeat-virtual-
// keys-design.md sec 12: Community's single role gives a key no shape
// distinct from every human's own access), and store.VirtualKey / store.
// GetVirtualKeyByClientID are themselves Enterprise-only
// (internal/store/virtual_key.ee.go, dropped from a generated Community
// tree) -- so this stub never touches the store at all. Every caller,
// including one whose token DOES carry a ClientID, answers false: no
// ClientID could ever match a virtual_key row, because no such row can
// exist in this edition.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and virtual_key_gate.ee.go,
// does not fail on a duplicate isVirtualKeyClient declaration; see
// routes_enterprise.community.go and
// TestCommunityRouteFileExcludedFromEnterpriseBuild for the toolchain-level
// proof of this shape. The generator renames this file to
// virtual_key_gate.go and drops virtual_key_gate.ee.go, so a generated
// Community tree compiles no reference to store.VirtualKey anywhere in
// internal/api.
func (s *Server) isVirtualKeyClient(ctx context.Context, clientID string) (bool, error) {
	return false, nil
}
