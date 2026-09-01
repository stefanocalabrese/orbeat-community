package api

// artifactManagerRole is the Community edition's half: empty, so the artifact
// routes accept `orbeat-admin` alone and nothing else changes.
//
// Community has no approval workflow at all (its admin_artifact_review.ee.go is
// dropped by the generator, and communityAutoApprove returns true), so a role
// whose purpose is "author and approve without being an admin" would grant the
// authoring half of a workflow whose approving half does not exist. A role that
// silently means less than its name says is worse than an absent one.
//
// The `community` build tag exists ONLY so this repo's own Enterprise build,
// which contains both files, does not fail on a duplicate declaration; the
// generator renames this file and drops the .ee.go twin.
func artifactManagerRole() string { return "" }
