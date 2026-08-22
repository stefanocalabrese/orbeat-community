package api

// editionLimits carries this edition's write-time caps (docs/specs/2026-08-19-
// orbeat-community-caps-design.md §4). Zero means unlimited, matching the
// existing convention for an "off" cap default elsewhere on Server
// (revisionKeep, auditExportMaxRows): a real deployment shipping "0 servers"
// or "0 roles" makes no sense, so 0 is unambiguous as a sentinel.
//
// Populated once, in New, from communityLimits() (limits.ee.go and
// limits.community.go, the extension-point pair, same shape as
// registerEnterpriseRoutes) and never changed outside a test in this package
// poking the field directly: the edition IS the build, not a runtime choice.
//
// The actual per-resource cap checks (checkServerActiveCap, checkRoleCap)
// live in caps.go: this type is the shared shape the extension point returns
// and the checks read from, so it has to compile on its own before either
// side exists.
type editionLimits struct {
	Servers int
	Roles   int
}
