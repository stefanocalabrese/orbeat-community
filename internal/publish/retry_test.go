package publish

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeOnce is a publisher whose outcome is scripted per attempt. Every call
// reports itself on calls, so tests wait on a SIGNAL rather than on a clock:
// a wall-clock assertion here would measure the machine, and this package's
// tests already run alongside six other CI jobs.
type fakeOnce struct {
	calls   chan int
	results []error // results[i] is attempt i+1's outcome; past the end means success
	n       int
}

func (f *fakeOnce) PublishOnce(context.Context) (Result, error) {
	f.n++
	n := f.n
	var err error
	if n-1 < len(f.results) {
		err = f.results[n-1]
	}
	f.calls <- n
	if err != nil {
		return Result{}, err
	}
	return Result{Commit: "deadbeef", Changed: true}, nil
}

// waitCall reads the next attempt number with a generous ceiling. The ceiling
// is a liveness bound, not a measurement: nothing here asserts how long a
// retry took, only that it happened at all.
func waitCall(t *testing.T, f *fakeOnce) int {
	t.Helper()
	select {
	case n := <-f.calls:
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no publish attempt arrived within the liveness ceiling")
		return 0
	}
}

// TestWorkerRetriesAfterFailureWithoutAnotherEnqueue is the whole point of the
// slice. Before it, a failed publish stayed failed until the next artifact
// mutation happened to enqueue another attempt, so a transient network failure
// against the git remote left the marketplace stale for as long as nobody
// touched the catalog. ONE Enqueue here, three attempts.
func TestWorkerRetriesAfterFailureWithoutAnotherEnqueue(t *testing.T) {
	f := &fakeOnce{calls: make(chan int, 8), results: []error{errors.New("push refused"), errors.New("push refused")}}
	done := make(chan Result, 8)
	w := NewWorker(f, time.Millisecond, func(_ context.Context, res Result, err error) {
		if err == nil {
			done <- res
		}
	})
	w.retryBase, w.retryMax = time.Millisecond, 5*time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	w.Enqueue()
	for want := 1; want <= 3; want++ {
		if got := waitCall(t, f); got != want {
			t.Fatalf("attempt %d arrived as %d", want, got)
		}
	}
	select {
	case res := <-done:
		if res.Attempt != 3 {
			t.Fatalf("the recovering run reported attempt %d, want 3 — an operator reading the audit trail cannot tell a first-try success from a recovery without it", res.Attempt)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the third attempt succeeded but onResult never saw it")
	}
}

// TestWorkerEnqueueInterruptsTheBackoffWait pins that a mutation during a long
// backoff is acted on rather than queued behind it. The retry delay is an hour,
// so this test can only pass if the enqueue actually interrupts the wait.
func TestWorkerEnqueueInterruptsTheBackoffWait(t *testing.T) {
	f := &fakeOnce{calls: make(chan int, 8), results: []error{errors.New("push refused")}}
	w := NewWorker(f, time.Millisecond, nil)
	w.retryBase, w.retryMax = time.Hour, time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	w.Enqueue()
	if got := waitCall(t, f); got != 1 {
		t.Fatalf("first attempt arrived as %d", got)
	}
	w.Enqueue()
	if got := waitCall(t, f); got != 2 {
		t.Fatalf("the enqueue during backoff produced attempt %d, want 2", got)
	}
}

// TestWorkerStopsRetryingOnCancel pins that a cancelled context ends the loop
// from inside the backoff wait, not only from the idle wait. A retry loop that
// ignored cancellation would keep publishing through shutdown, which is the
// one thing cmd/api's ordered shutdown exists to prevent.
func TestWorkerStopsRetryingOnCancel(t *testing.T) {
	f := &fakeOnce{calls: make(chan int, 8), results: []error{errors.New("push refused")}}
	stopped := make(chan struct{})
	w := NewWorker(f, time.Millisecond, nil)
	w.retryBase, w.retryMax = time.Hour, time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	go func() { w.Start(ctx); close(stopped) }()

	w.Enqueue()
	waitCall(t, f)
	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after its context was cancelled during a backoff wait")
	}
}

// TestNextRetryDelay is a pure-function test, deliberately: the schedule is the
// part worth pinning exactly, and pinning it through the loop would mean timing
// a real wait.
func TestNextRetryDelay(t *testing.T) {
	base, max := 5*time.Second, 30*time.Minute
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{0, 5 * time.Second}, // defensive: never a zero-length wait
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{9, 21*time.Minute + 20*time.Second},
		{10, 30 * time.Minute}, // the cap, reached rather than overshot
		{50, 30 * time.Minute}, // and held, with no overflow from doubling
	} {
		if got := nextRetryDelay(tc.failures, base, max); got != tc.want {
			t.Errorf("nextRetryDelay(%d) = %s, want %s", tc.failures, got, tc.want)
		}
	}
}
