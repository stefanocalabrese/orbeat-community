// Package version holds the single build-time version string every orbeat
// binary — and the OpenAPI document orbeat-api serves — reports.
//
// Version defaults to "dev". That is not a placeholder to fix later: it is
// the correct, useful answer for every local/CI build that isn't the release
// pipeline, and it is what makes an unwired --version flag or a typo'd
// linker symbol visible instead of silently claiming to be a real release.
//
// The release build overrides it via the linker, e.g.:
//
//	go build -ldflags "-X github.com/stefanocalabrese/orbeat-community/internal/version.Version=1.25.0" ./cmd/sync
//
// This must be the ONLY place the version is read from: three consumers
// (orbeat-sync --version, the gateway's advertised MCP Implementation
// version, and orbeat-api's served openapi.yaml info.version) all resolve it
// from here rather than carrying their own copy, which is exactly how the
// gateway's copy went stale at a hardcoded "0.3.0" while the product shipped
// v1.25.0 (fable-audit §7 #15).
package version

// Version is the orbeat release version this binary was built from.
// Overridden at release build time via -ldflags -X (see package doc); "dev"
// for any build that did not set it, including every local/dev build.
var Version = "dev"
