package secrets

// registerEnterpriseProviders is the Community no-op counterpart to
// enterprise.ee.go's real implementation (docs/specs/2026-08-19-orbeat-
// community-repo-generation-design.md §4). The generator renames this file
// to enterprise.go and drops enterprise.ee.go, so a generated Community tree
// never registers vault:/awssm: providers — only env: is available.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build — which still contains both this file and enterprise.ee.go — does
// not fail on duplicate declarations; a generated Community tree, having no
// .ee.go file left to collide with, compiles this unconditionally, tag
// included. See internal/api/routes_enterprise.community.go for the
// toolchain-level proof this file plays no part in a normal `go build`
// (TestCommunityRouteFileExcludedFromEnterpriseBuild) — the same mechanism
// applies here.
func registerEnterpriseProviders(providers map[string]SecretsProvider) {}

// enterpriseSchemes is the Community no-op counterpart to enterprise.ee.go's
// real implementation: Community registers no Enterprise-only schemes.
func enterpriseSchemes() []string { return nil }
