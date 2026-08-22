package authz

// communitySeatLimit is the Community edition's real seat cap (spec §4): 10
// active seats. The generator renames this file to seatlimit.go and drops
// seatlimit.ee.go, so a generated Community tree enforces this
// unconditionally.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and seatlimit.ee.go, does
// not fail on a duplicate communitySeatLimit declaration; see
// internal/api/routes_enterprise.community.go and
// TestCommunityRouteFileExcludedFromEnterpriseBuild for the toolchain-level
// proof this file plays no part in a normal `go build`.
func communitySeatLimit() int {
	return 10
}
