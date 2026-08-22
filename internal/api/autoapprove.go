package api

// communityAutoApprove is the Community edition's real half of the extension
// point (see autoapprove.ee.go's doc comment for the shared contract). true:
// the generated Community tree has no approval workflow at all (its
// admin_artifact_review.ee.go is dropped by the generator), so without
// immediate auto-approval nothing an admin creates or edits is ever served by
// GET /v1/sync/artifacts or the marketplace publisher: both filter on
// approved_content IS NOT NULL (spec §2: "every distribution channel is
// dead").
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and autoapprove.ee.go, does not
// fail on a duplicate communityAutoApprove declaration; see
// internal/api/routes_enterprise.community.go and
// TestCommunityRouteFileExcludedFromEnterpriseBuild for the toolchain-level
// proof this file plays no part in a normal `go build`. The generator renames
// this file to autoapprove.go and drops autoapprove.ee.go, so a generated
// Community tree enforces this unconditionally.
func communityAutoApprove() bool { return true }
