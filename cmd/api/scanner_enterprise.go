package main

import (
	"context"

	"github.com/stefanocalabrese/orbeat-community/internal/config"
	"github.com/stefanocalabrese/orbeat-community/internal/govern"
	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
)

// buildScanner is the Community no-op counterpart to
// scanner_enterprise.ee.go's real implementation (docs/specs/2026-08-19-
// orbeat-community-repo-generation-design.md §4). Community carries no LLM
// scanner, so any ORBEAT_SCAN_LLM_* configuration is inert and base (the
// rules scanner) is returned unchanged.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build — which still contains both this file and scanner_enterprise.ee.go —
// does not fail on duplicate declarations; see
// internal/api/routes_enterprise.community.go for the toolchain-level proof
// this file plays no part in a normal `go build`.
func buildScanner(ctx context.Context, cfg config.Config, secretsResolver *secrets.Resolver, base govern.Scanner) (govern.Scanner, error) {
	return base, nil
}
