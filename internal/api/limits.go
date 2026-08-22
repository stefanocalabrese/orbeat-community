package api

// communityLimits is the Community edition's real write-time caps (spec §4):
// 10 active MCP servers, 1 custom role. The generator renames this file to
// limits.go and drops limits.ee.go, so a generated Community tree enforces
// these unconditionally.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and limits.ee.go, does not
// fail on a duplicate communityLimits declaration; see
// routes_enterprise.community.go and TestCommunityRouteFileExcludedFromEnterpriseBuild
// for the toolchain-level proof this file plays no part in a normal `go build`.
func communityLimits() editionLimits {
	return editionLimits{Servers: 10, Roles: 1}
}
