package api

import (
	"context"
	"log/slog"
	"time"
)

// auditPruner deletes audit rows older than cutoff, in batches; matches
// store.Store.PruneAuditEventsOlderThan (injected so it's testable).
type auditPruner func(ctx context.Context, cutoff time.Time, batch int) (int64, error)

const auditPruneBatch = 10000

// pruneAuditOnce computes the cutoff (now - retentionDays) and prunes once.
func pruneAuditOnce(ctx context.Context, prune auditPruner, retentionDays, batch int) (int64, error) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	return prune(ctx, cutoff, batch)
}

// RunAuditRetention prunes audit rows older than retentionDays every interval,
// until ctx is cancelled. It runs one prune shortly after start, then on the
// ticker. The caller guarantees retentionDays > 0 (off-by-default is enforced by
// the caller not starting this goroutine at all).
func RunAuditRetention(ctx context.Context, prune auditPruner, retentionDays int, interval time.Duration) {
	run := func() {
		n, err := pruneAuditOnce(ctx, prune, retentionDays, auditPruneBatch)
		if err != nil {
			slog.Error("audit retention prune failed", "err", err)
			return
		}
		if n > 0 {
			slog.Info("audit retention pruned", "event", "audit_retention", "deleted", n, "older_than_days", retentionDays)
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
