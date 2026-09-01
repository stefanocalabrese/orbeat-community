package api

// pinningSupported is the Community edition's half of the extension point (see
// pinning.ee.go's doc comment for the shared contract). false: ?pin is ignored
// entirely, the three pinning fields are omitted from every sync artifact, and
// GET /v1/sync/config says pinning: false.
//
// IGNORED, NOT REJECTED, and that asymmetry with the malformed-input 400s is
// the point. handleSyncArtifacts is a SHARED handler, so a Community build
// still compiles the whole pin path; what this value does is stop it running.
// A Community server that 400'd a new client's ?pin= would turn a
// warn-and-continue into a broken sync for a developer who did nothing wrong,
// on a server she has no way to change.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise) build,
// which still contains both this file and pinning.ee.go, does not fail on a
// duplicate declaration; see internal/api/routes_enterprise.community.go and
// TestCommunityRouteFileExcludedFromEnterpriseBuild for the toolchain-level
// proof this file plays no part in a normal `go build`. The generator renames
// this file to pinning.go and drops pinning.ee.go, so a generated Community
// tree compiles no other value.
func pinningSupported() bool { return false }
