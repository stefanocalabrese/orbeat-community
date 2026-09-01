package api

// virtualKeysSupported is the Community edition's half of the extension
// point (see virtual_keys.ee.go's doc comment for the shared contract).
// false: a generated Community tree has no virtual-key routes at all
// (registerEnterpriseRoutes is a no-op there, routes_enterprise.community.go),
// and this value is what stops GET /v1/me, which IS shared, from
// advertising a console page that build does not serve.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and virtual_keys.ee.go, does
// not fail on a duplicate virtualKeysSupported declaration; see
// internal/api/routes_enterprise.community.go and
// TestCommunityRouteFileExcludedFromEnterpriseBuild for the toolchain-level
// proof this file plays no part in a normal `go build`. The generator
// renames this file to virtual_keys.go and drops virtual_keys.ee.go, so a
// generated Community tree compiles no other value.
func virtualKeysSupported() bool { return false }
