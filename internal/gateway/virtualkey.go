package gateway

import (
	"context"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
)

// resolveVirtualKey is the Community edition's half of buildSession's
// identity-resolution extension point (see virtualkey.ee.go's doc comment
// for the shared contract and why this pairing exists at all). Virtual keys
// are Enterprise-only (docs/specs/2026-08-25-orbeat-virtual-keys-design.md
// §12: Community's single role gives a key no shape distinct from every
// human's own access), and store.VirtualKey / store.GetVirtualKeyByClientID
// are themselves Enterprise-only (internal/store/virtual_key.ee.go, dropped
// from a generated Community tree) -- so this stub never touches the store
// at all. Every caller, including one whose token DOES carry a ClientID,
// falls straight through to buildSession's human path.
//
// That includes a SERVICE ACCOUNT, which the Enterprise half has refused
// since 2026-08-30 when it names no virtual_key row. The editions diverge
// here on purpose. That refusal exists because a destroyed virtual key must
// not silently become a person, and Community has no virtual keys to
// destroy, so every service account reaching a Community gateway was always
// something else. Refusing them here would be a new rule about non-orbeat
// robots rather than the closing of a hole, and it would be one a Community
// operator never asked for. What Community keeps from today's behaviour is
// therefore also today's cost: such a token still resolves as a person and
// still holds one of the ten active seats for seven days.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and virtualkey.ee.go, does not
// fail on a duplicate resolveVirtualKey declaration; see
// internal/api/routes_enterprise.community.go and
// TestCommunityRouteFileExcludedFromEnterpriseBuild for the toolchain-level
// proof of this shape. The generator renames this file to virtualkey.go and
// drops virtualkey.ee.go, so a generated Community tree compiles no
// reference to store.VirtualKey anywhere in internal/gateway.
func (s *Server) resolveVirtualKey(ctx context.Context, p auth.Principal) (rc authz.ResolvedContext, keyID string, keyNarrow []string, ok bool, err error) {
	return authz.ResolvedContext{}, "", nil, false, nil
}

// keyRevoked is the Community stub for Server.keyRevoked
// (virtualkey.ee.go), following the exact same pairing as
// resolveVirtualKey above. Because resolveVirtualKey never returns ok=true
// in a Community build, sess.keyID is always empty, and rbac_middleware.go
// (SHARED) never calls this with a real key -- its return value is
// therefore never observed, but the signature must match so that shared
// file compiles against both editions.
func (s *Server) keyRevoked(ctx context.Context, tenantID, clientID string) (bool, error) {
	return false, nil
}
