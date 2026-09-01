package api

import "net/http"

// registerEnterpriseRoutes is the Community no-op counterpart to
// routes_enterprise.ee.go's real implementation (docs/specs/2026-08-19-
// orbeat-community-repo-generation-design.md §4). The generator renames this
// file to routes_enterprise.go and drops routes_enterprise.ee.go, so a
// generated Community tree never wires the artifact review lifecycle,
// revision history/rollback, audit export, the deployment registry report
// route, or the artifact minimum-revision floor. api.go names no Enterprise
// handler either way, so there is nothing else to strip.
//
// It takes authed as well as admin purely to match the Enterprise signature.
// The parameter is unused here for the same reason admin is: this half
// registers nothing. Its presence is what makes adding an authed(...) route
// to the Enterprise half a compile error here rather than a silently
// unregistered route there.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build — which still contains both this file and routes_enterprise.ee.go —
// does not fail on a duplicate registerEnterpriseRoutes declaration; a
// generated Community tree, having no .ee.go file left to collide with,
// would compile this unconditionally, tag included. See
// TestCommunityRouteFileExcludedFromEnterpriseBuild for the toolchain-level
// proof that this file plays no part in a normal `go build`.
func (s *Server) registerEnterpriseRoutes(mux *http.ServeMux, admin, curated, authed func(http.HandlerFunc) http.Handler) {
}

// enterprisePaginatedOps, enterpriseSortableOps, enterpriseSearchableOps and
// enterpriseGuardedOps are the Community no-op counterparts to
// routes_enterprise.ee.go's real implementations: Community registers no
// Enterprise-only routes, so all four return nil.
func enterprisePaginatedOps() []string  { return nil }
func enterpriseSortableOps() []string   { return nil }
func enterpriseSearchableOps() []string { return nil }
func enterpriseGuardedOps() []string    { return nil }
