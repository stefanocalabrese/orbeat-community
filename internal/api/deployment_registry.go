package api

// deploymentRegistrySupported is the Community edition's half of the extension
// point (see deployment_registry.ee.go's doc comment for the shared contract).
// false: a generated Community tree has no report route at all, because
// registerEnterpriseRoutes is a no-op there (routes_enterprise.community.go),
// and this value is what stops handleSyncConfig, which IS shared, from
// advertising a route that build does not serve. An operator who sets
// ORBEAT_DEPLOYMENT_REGISTRY here gets false, not a 404 they have to diagnose.
//
// The edition answer is deliberate rather than incidental (spec sec 11): a
// per-developer per-machine record is the edition question where a wrong
// answer has a privacy cost and not just a revenue one, and Community is the
// edition an organisation runs before it has decided anything about employee
// data.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise) build,
// which still contains both this file and deployment_registry.ee.go, does not
// fail on a duplicate declaration; see internal/api/routes_enterprise.
// community.go and TestCommunityRouteFileExcludedFromEnterpriseBuild for the
// toolchain-level proof this file plays no part in a normal `go build`. The
// generator renames this file to deployment_registry.go and drops
// deployment_registry.ee.go, so a generated Community tree compiles no other
// value.
func deploymentRegistrySupported() bool { return false }
