package main

import (
	"github.com/stefanocalabrese/orbeat-community/internal/config"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// startUsageRetention is the Community no-op counterpart to
// usage_retention.ee.go's real implementation (docs/specs/2026-08-19-orbeat-
// community-repo-generation-design.md sec 4). A Community build has no
// usage metering at all: internal/gateway's UsageCounter is the no-op stub
// (usage.community.go), so nothing ever writes a usage_daily row. The
// migration that creates usage_daily still runs in a generated Community
// tree (goose migrations are plain SQL, unaffected by the .ee.go/.community.go
// convention -- only *.go files are), so the table itself DOES exist there;
// it simply always stays empty, since nothing in a Community build can ever
// insert into it. nil means "no loop started", which main.go already
// handles for audit and deployment retention.
//
// ORBEAT_USAGE_RETENTION_DAYS is therefore inert here rather than wrong:
// the knob parses, its default is still 90, and no loop reads it. There is
// deliberately no startup warning about that, mirroring
// deployment_retention.community.go's own reasoning: a Community operator
// who never had usage rows in the first place has nothing to be warned
// about.
//
// The `community` build tag exists ONLY so this repo's own (Enterprise)
// build, which still contains both this file and usage_retention.ee.go,
// does not fail on a duplicate declaration.
func startUsageRetention(st *store.Store, cfg config.Config) func() { return nil }
