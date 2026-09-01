package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/syncclient"
)

// watchSync is tested through a seam rather than through a real sync: the
// question it answers is "when does the loop stop", and driving it with real
// HTTP and a real filesystem would test everything except that.
//
// The tests below wait on SIGNALS from the fake run (a channel) with a generous
// liveness ceiling, never on elapsed wall-clock time. A test asserting "it slept
// about 15 minutes" would measure the machine, and this package's tests run
// alongside six other CI jobs.

// runsUntil returns a syncOnce stand-in that reports each call on calls and
// returns results[i] for call i (the last result repeats).
func runsUntil(calls chan<- int, results ...error) func() error {
	var n int64
	return func() error {
		i := int(atomic.AddInt64(&n, 1)) - 1
		calls <- i + 1
		if i >= len(results) {
			i = len(results) - 1
		}
		return results[i]
	}
}

func TestWatchStopsOnAFatalRun(t *testing.T) {
	calls := make(chan int, 8)
	fatal := &exitError{code: 2, err: errors.New("tampered manifest")}
	done := make(chan error, 1)
	go func() {
		done <- watchLoop(context.Background(), time.Millisecond, runsUntil(calls, nil, fatal))
	}()

	select {
	case n := <-calls:
		if n != 1 {
			t.Fatalf("first call reported as %d", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first sync never ran")
	}
	select {
	case err := <-done:
		// The clean first run must NOT have stopped it, so reaching here means
		// the second (fatal) run did.
		if exitCode(err) != 2 {
			t.Fatalf("watch returned exit code %d, want 2 — a fatal run must not be retried", exitCode(err))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch kept running after a fatal run; exit 2 means do NOT retry")
	}
}

func TestWatchKeepsGoingAfterAPartialRun(t *testing.T) {
	calls := make(chan int, 16)
	partial := &exitError{code: 1, err: errors.New("one project failed")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watchLoop(ctx, time.Millisecond, runsUntil(calls, partial)) }()

	// Three consecutive partial runs: exit 1 is the RETRYABLE half of the
	// contract, so the loop must treat it as an ordinary tick.
	for want := 1; want <= 3; want++ {
		select {
		case n := <-calls:
			if n != want {
				t.Fatalf("call %d arrived as %d", want, n)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("watch stopped after %d partial run(s)", want-1)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a cancelled watch must end cleanly, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not return after its context was cancelled")
	}
}

func TestWatchRunsImmediatelyNotAfterAnInterval(t *testing.T) {
	calls := make(chan int, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An hour-long interval: the first run can only arrive if the loop syncs
	// before its first tick.
	go func() { _ = watchLoop(ctx, time.Hour, runsUntil(calls, nil)) }()
	select {
	case <-calls:
	case <-time.After(10 * time.Second):
		t.Fatal("watch waited for a full interval before its first sync")
	}
}

// TestSyncRejectsATooShortInterval covers the floor, which the loop tests
// cannot: they call watchLoop directly with whatever interval they like, so a
// deleted validation left them all green.
//
// The floor is not arbitrary politeness. A one-second watch would hold the run
// lock almost continuously, and that is the SAME lock `project add` and
// `connect` take, so those commands would block for a reason a developer has no
// way to see.
//
// Note the shape of its red: deleting the validation does not make this test
// FAIL, it makes it HANG, because runSync then falls through to a real watch
// loop against a real API. Go's -timeout turns that into a failure, so the gate
// holds, but a run that stops printing for ten minutes is the symptom to expect
// rather than a neat assertion message.
// B22: a partial (exit 1) run's error must be printed, not silently dropped.
// Before this fix, watchLoop checked `err` only for exitCode()==2 and then
// discarded it outright — so a watch whose session expired, whose API was
// unreachable, or that was blocked behind another run's lock printed
// NOTHING, every interval, forever: `err` for exit 0 and exit 1 was simply
// never looked at again. syncOnce's own early failures (loadValidToken,
// FetchArtifacts, acquireRunLock) return before ever reaching
// renderHuman/renderJSON, so there was no report anywhere for a watching
// developer to read either.
func TestWatchPrintsAPartialRunsError(t *testing.T) {
	calls := make(chan int, 4)
	partial := &exitError{code: 1, err: errors.New("session expired; run 'orbeat-sync login'")}
	ctx, cancel := context.WithCancel(context.Background())

	out := captureStderr(t, func() {
		go func() {
			// Cancel right after the first (partial) run is observed, so the
			// loop returns quickly instead of waiting out the fake interval.
			<-calls
			cancel()
		}()
		_ = watchLoop(ctx, time.Hour, runsUntil(calls, partial))
	})
	if !strings.Contains(out, "session expired; run 'orbeat-sync login'") {
		t.Fatalf("a partial (exit 1) run's error must be printed every iteration, not silently dropped; stderr=%q", out)
	}
	if !strings.Contains(out, "orbeat-sync:") {
		t.Fatalf("the printed line must match main()'s own format (\"orbeat-sync: <err>\"), got stderr=%q", out)
	}
}

// A clean (exit 0, nil err) run must print nothing — there is nothing to
// report, and printing a blank/empty line every interval would just be noise.
func TestWatchPrintsNothingOnACleanRun(t *testing.T) {
	calls := make(chan int, 4)
	ctx, cancel := context.WithCancel(context.Background())

	out := captureStderr(t, func() {
		go func() {
			<-calls
			cancel()
		}()
		_ = watchLoop(ctx, time.Hour, runsUntil(calls, nil))
	})
	if out != "" {
		t.Fatalf("a clean run must print nothing, got stderr=%q", out)
	}
}

func TestSyncRejectsATooShortInterval(t *testing.T) {
	for _, arg := range []string{"1s", "59s", "0s"} {
		err := runSync(context.Background(), syncclient.Config{}, []string{"--watch", "--interval", arg})
		if err == nil {
			t.Fatalf("--interval %s was accepted", arg)
		}
		if !strings.Contains(err.Error(), "at least") {
			t.Fatalf("--interval %s failed with %v, which does not name the floor", arg, err)
		}
	}
}
