package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestPruneOnceComputesTheCutoff covers the cutoff arithmetic both retention
// loops share (runRetention is the only caller of pruneOnce, for either
// subject), which is why the helper is no longer named for audit alone.
func TestPruneOnceComputesTheCutoff(t *testing.T) {
	var gotCutoff time.Time
	var gotBatch int
	prune := func(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
		gotCutoff, gotBatch = cutoff, batch
		return 3, nil
	}
	before := time.Now().Add(-30 * 24 * time.Hour)
	n, err := pruneOnce(context.Background(), prune, 30, 500)
	after := time.Now().Add(-30 * 24 * time.Hour)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v, want 3,nil", n, err)
	}
	if gotBatch != 500 {
		t.Errorf("batch=%d, want 500", gotBatch)
	}
	if gotCutoff.Before(before) || gotCutoff.After(after) {
		t.Errorf("cutoff %v not ~now-30d (want between %v and %v)", gotCutoff, before, after)
	}
}

// TestRunAuditRetentionLoop covers the ticker loop the goroutine actually runs:
// it prunes once immediately + on each tick, keeps going after a prune error,
// and exits cleanly when the context is cancelled (no leak).
func TestRunAuditRetentionLoop(t *testing.T) {
	var calls atomic.Int64
	prune := func(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
		if calls.Add(1) == 2 {
			return 0, errors.New("boom") // a mid-run error must NOT stop the loop
		}
		return 1, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunAuditRetention(ctx, prune, 7, 5*time.Millisecond) // immediate run + fast ticks
		close(done)
	}()

	// Immediate run + several ticks (incl. the erroring 2nd call) → well past 3 calls.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("loop made only %d calls, want >=3 (ticker or error-continue broken)", calls.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	select {
	case <-done: // clean exit on ctx cancel
	case <-time.After(2 * time.Second):
		t.Fatal("RunAuditRetention did not return after ctx cancel (goroutine leak)")
	}
}
