package main

import (
	"github.com/stefanocalabrese/orbeat-community/internal/config"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// startDeploymentRetention is the Community no-op counterpart to
// deployment_retention.ee.go's real implementation (docs/specs/2026-08-19-
// orbeat-community-repo-generation-design.md sec 4). A Community build has no
// artifact deployment registry at all: deploymentRegistrySupported() is false
// (internal/api/deployment_registry.community.go), registerEnterpriseRoutes is
// a no-op, so nothing ever writes an artifact_deployment row and there is
// nothing to prune. nil means "no loop started", which main.go already handles
// for audit retention.
//
// ORBEAT_DEPLOYMENT_RETENTION_DAYS is therefore inert here rather than
// wrong: the knob parses, its default is still 90, and no loop reads it. There
// is deliberately no startup warning about that, because a Community operator
// who never turned a registry on has nothing to be warned about.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise) build,
// which still contains both this file and deployment_retention.ee.go, does not
// fail on a duplicate declaration; see internal/api/routes_enterprise.
// community.go for the toolchain-level proof this file plays no part in a
// normal `go build`.
func startDeploymentRetention(st *store.Store, cfg config.Config) func() { return nil }
