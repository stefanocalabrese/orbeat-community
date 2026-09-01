package gateway

import (
	"context"
	"log/slog"
	"time"
)

// usageFlushFunc and quotaRefreshFunc are the two operations
// RunUsageTicker drives, injected as bare func values -- the same
// injection shape internal/api/retention.go's retentionPruner uses, and for
// the same reason: the loop below becomes testable with a plain closure,
// with no real *store.Store, no UsageCounter and no QuotaEnforcer required.
// In production these are always (*UsageCounter).Flush and
// (*QuotaEnforcer).RefreshAll method values -- cmd/gateway/main.go's call
// site passes uc.Flush and qe.RefreshAll directly, which satisfy these
// signatures without either edition needing its own adapter: both editions'
// UsageCounter/QuotaEnforcer types declare Flush/RefreshAll with this exact
// shape (usage.ee.go/usage.community.go, quota.ee.go/quota.community.go).
type usageFlushFunc func(ctx context.Context) error
type quotaRefreshFunc func(ctx context.Context) error

// RunUsageTicker periodically flushes accumulated usage counts and refreshes
// the quota cache, until ctx is cancelled. It runs once immediately (don't
// wait a full interval before the very first flush/refresh), mirroring
// internal/api/retention.go's runRetention -- and for the SAME two reasons
// that loop states plus a third specific to quota enforcement: a freshly
// started gateway would otherwise run for a full interval with an EMPTY
// quota cache, during which QuotaEnforcer.Check treats every role as
// unlimited (quota.ee.go's documented "no cache entry is unlimited"
// contract) regardless of what any role_quota row says -- the immediate
// first run closes that gap rather than leaving it open for `interval`.
//
// A flush or refresh error is logged and the loop continues, exactly like
// runRetention's own reasoning: a transient database failure must not
// silently end metering (or quota enforcement) for the rest of the
// process's life. flush and refresh are called independently -- a flush
// failure does not skip that same tick's refresh, and vice versa, so one
// side's outage never masks the other's.
//
// THIS IS DELIBERATELY NOT WHERE THE SHUTDOWN FLUSH LIVES. This loop's own
// exit (ctx.Done()) performs NO final flush -- cmd/gateway/main.go issues
// one explicit, separate uc.Flush call after stopping this loop, on its own
// context (main.go's run() function, not this one). That split is what lets
// cmd/gateway/usage_wiring_test.go's go/ast gate assert "run() flushes on
// shutdown" unambiguously: the only uc.Flush call site visible to an AST
// walk of run() is that deliberate shutdown call, because this loop's own
// periodic Flush call is inside THIS function, in a different file and
// package, invisible to that scan.
func RunUsageTicker(ctx context.Context, flush usageFlushFunc, refresh quotaRefreshFunc, interval time.Duration) {
	run := func() {
		if err := flush(ctx); err != nil {
			slog.Error("usage flush failed", "err", err)
		}
		if err := refresh(ctx); err != nil {
			slog.Error("quota cache refresh failed", "err", err)
		}
	}
	run() // once on start (don't wait a full interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
