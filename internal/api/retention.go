package api

import (
	"context"
	"log/slog"
	"time"
)

// retentionPruner deletes rows older than cutoff, in batches, and returns how
// many it deleted. Both store.Store.PruneAuditEventsOlderThan and
// store.Store.PruneDeploymentsOlderThan have exactly this shape; it is
// injected rather than reached through a *store.Store so the loop below is
// testable without a database.
//
// The type stays edition-neutral even though one of its two implementations
// is Enterprise-only: it names no store method, so a generated Community tree
// carries this file unchanged and drops only RunDeploymentRetention
// (retention.ee.go).
type retentionPruner func(ctx context.Context, cutoff time.Time, batch int) (int64, error)

// retentionPruneBatch bounds one DELETE statement. Both pruners treat it the
// same way (a batch <= 0 falls back to their own 10000), and both loop until
// a short batch drains the backlog, so this only sets how long a single
// statement holds its locks.
const retentionPruneBatch = 10000

// pruneOnce computes the cutoff (now - retentionDays) and prunes once. Shared
// by both retention loops: the cutoff arithmetic is the same subtraction
// whichever table it aims at.
func pruneOnce(ctx context.Context, prune retentionPruner, retentionDays, batch int) (int64, error) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	return prune(ctx, cutoff, batch)
}

// runRetention prunes rows older than retentionDays every interval, until ctx
// is cancelled. It runs one prune shortly after start, then on the ticker. A
// prune error is logged and the loop continues: a transient database failure
// must not silently end retention for the rest of the process's life.
//
// The caller guarantees retentionDays > 0. WHETHER retention runs at all is
// the caller's decision and the two callers make it in opposite directions,
// which is the whole substantive difference between them: audit retention is
// off unless the operator sets a window (ORBEAT_AUDIT_RETENTION_DAYS defaults
// to 0, because an audit row is a compliance record and deleting it is a
// loss, and the prod backup sidecar is the archive), deployment retention is
// on at 90 days unless the operator sets 0 (ORBEAT_DEPLOYMENT_RETENTION_DAYS,
// because a deployment row is a claim about a person's machine that decays,
// and a stale one is a wrong answer rather than a missing record).
//
// name is the subject of every log line this loop writes ("audit",
// "deployment") and is the only thing that differs between the two entry
// points below. It is a parameter rather than two copies of this ticker
// because a second hand-copied loop is the defect class this repo keeps
// paying for; the log strings are assembled so that the audit lines are
// byte-identical to the ones this file emitted before the extraction, since
// an operator's alert may match on them.
func runRetention(ctx context.Context, name string, prune retentionPruner, retentionDays int, interval time.Duration) {
	run := func() {
		n, err := pruneOnce(ctx, prune, retentionDays, retentionPruneBatch)
		if err != nil {
			slog.Error(name+" retention prune failed", "err", err)
			return
		}
		if n > 0 {
			slog.Info(name+" retention pruned", "event", name+"_retention", "deleted", n, "older_than_days", retentionDays)
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

// RunAuditRetention prunes audit rows older than retentionDays every interval,
// until ctx is cancelled. It runs one prune shortly after start, then on the
// ticker. The caller guarantees retentionDays > 0 (off-by-default is enforced
// by the caller not starting this goroutine at all).
func RunAuditRetention(ctx context.Context, prune retentionPruner, retentionDays int, interval time.Duration) {
	runRetention(ctx, "audit", prune, retentionDays, interval)
}
