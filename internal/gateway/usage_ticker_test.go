package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunUsageTickerRunsImmediatelyThenOnInterval pins the "don't wait a
// full interval" contract: with a long interval, BOTH flush and refresh
// must have already run once by the time this returns, well before the
// ticker itself would ever fire.
func TestRunUsageTickerRunsImmediatelyThenOnInterval(t *testing.T) {
	// SIGNALLED, NOT TIMED, matching its sibling below. This one is the milder
	// case (it waits for ONE startup call, not three ticked ones), but the
	// shape is identical and the shape is what fails under load: a wall-clock
	// deadline turns "the machine was busy" into a red that reads as "the
	// ticker did not run immediately". The interval here is an hour, so
	// anything that arrives at all is the immediate call by construction.
	flushed := make(chan struct{}, 1)
	refreshed := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunUsageTicker(ctx, func(context.Context) error {
			select {
			case flushed <- struct{}{}:
			default: // never block the loop under test
			}
			return nil
		}, func(context.Context) error {
			select {
			case refreshed <- struct{}{}:
			default:
			}
			return nil
		}, time.Hour)
		close(done)
	}()

	select {
	case <-flushed:
	case <-time.After(30 * time.Second):
		t.Fatal("no flush arrived: RunUsageTicker must flush once immediately, not wait a full interval")
	}
	select {
	case <-refreshed:
	case <-time.After(30 * time.Second):
		t.Fatal("no refresh arrived: RunUsageTicker must refresh once immediately, not wait a full interval")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("RunUsageTicker did not return after its context was cancelled")
	}
}

// TestRunUsageTickerContinuesAfterFlushError proves a flush failure does not
// end the loop, and does not skip that same tick's refresh -- exactly
// internal/api/retention.go's runRetention resilience contract, applied to
// both halves of this loop independently.
func TestRunUsageTickerContinuesAfterFlushError(t *testing.T) {
	// SIGNALLED, NOT TIMED. The first version of this test ran a 20ms ticker
	// against a 2s wall-clock deadline and demanded 3 flushes. That passes on
	// an idle machine and FAILED inside the pre-push matrix, where six other
	// jobs are competing for the same cores: 2 seconds of wall clock is not 2
	// seconds of scheduling. Retrying until it passed would have left a test
	// whose red says "the machine was busy" rather than "the loop stopped".
	//
	// So the flush closure now reports each call on a channel and the test
	// waits for three of them with a generous ceiling. Under load it takes
	// longer and still passes; if the loop actually stops on the error, it
	// blocks and fails for the one reason it exists.
	flushes := make(chan struct{}, 8)
	var refreshCalls int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		RunUsageTicker(ctx, func(context.Context) error {
			select {
			case flushes <- struct{}{}:
			default: // never block the loop under test
			}
			return errors.New("boom")
		}, func(context.Context) error {
			atomic.AddInt32(&refreshCalls, 1)
			return nil
		}, 20*time.Millisecond)
		close(done)
	}()

	for i := range 3 {
		select {
		case <-flushes:
		case <-time.After(30 * time.Second):
			t.Fatalf("flush %d of 3 never arrived: a flush error ended the ticker loop, so usage stops being written after the first failure", i+1)
		}
	}
	// Each tick refreshes the quota cache AFTER flushing, so three flushes
	// imply at least two completed refreshes; the third may still be in
	// flight when the third flush is observed.
	if got := atomic.LoadInt32(&refreshCalls); got < 2 {
		t.Fatalf("refreshCalls = %d, want >= 2 -- a flush error on one tick must not skip that same tick's refresh", got)
	}
}
