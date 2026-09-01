package authz

// communitySeatLimit is the Community edition's real seat cap (spec §4): 10
// active seats. The generator renames this file to seatlimit.go and drops
// seatlimit.ee.go, so a generated Community tree enforces this
// unconditionally.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and seatlimit.ee.go, does
// not fail on a duplicate communitySeatLimit declaration.
//
// The proof that this file plays no part in a normal `go build` is that
// package authz compiles at all: lose the tag and the two declarations of
// communitySeatLimit collide, so every build and every test in the package
// fails outright rather than quietly picking one. Which of the two survived
// is then pinned by TestCommunitySeatLimitIsUnlimitedInThisBuild
// (seatlimit.ee_test.go), asserting the value is 0.
//
// internal/api/routes_enterprise.community.go carries the same tag for the
// same reason, and TestCommunityRouteFileExcludedFromEnterpriseBuild
// demonstrates the constraint's effect through go/build's own evaluator. It
// says nothing about THIS file, and was cited here as though it did until
// 2026-08-28: it calls build.ImportDir(".") from internal/api and asserts on
// three routes_enterprise.* filenames, never reading internal/authz.
func communitySeatLimit() int {
	return 10
}
