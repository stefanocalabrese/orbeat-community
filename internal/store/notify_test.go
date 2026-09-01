package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// listenTestStore opens a Store whose pool holds exactly ONE connection, so
// the connection Listen used is the only one the pool can hand back. Without
// that cap the assertion below would be a coin flip: the pool could serve a
// fresh connection and the test would pass while the subscribed one sat in the
// pool behind it.
func listenTestStore(t *testing.T) *Store {
	t.Helper()
	sep := "?"
	if strings.Contains(testDSN, "?") {
		sep = "&"
	}
	s, err := New(context.Background(), testDSN+sep+"pool_max_conns=1")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// poolListeningChannels asks the pool for a connection and reports what THAT
// connection is subscribed to. pg_listening_channels() is session-scoped, so
// this is a direct question about the connection the pool just handed out.
func poolListeningChannels(t *testing.T, s *Store) []string {
	t.Helper()
	var chans []string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT coalesce(array_agg(c), '{}') FROM pg_listening_channels() AS c`,
	).Scan(&chans); err != nil {
		t.Fatalf("pg_listening_channels: %v", err)
	}
	return chans
}

// TestListenNeverReturnsASubscribedConnectionToThePool pins the invariant
// Listen's own doc comment states: the connection it borrows carries LISTEN
// state that no other borrower may inherit.
//
// This is a regression test for a REPRODUCED defect, not a hypothetical.
// Listen used to end with `defer conn.Release()`, and pgxpool's Release
// destroys a connection only when it is closed, busy, mid-transaction or past
// its lifetime (pgxpool/conn.go). A cancelled listener leaves the connection
// healthy and idle on every path, so Release handed it back to the pool still
// subscribed. Both cancellation paths were measured leaking, and both are
// asserted here because they end Listen through different code: one unwinds
// from inside WaitForNotification, the other never re-enters it.
//
// Why it mattered: an idle pooled connection reads nothing, so Postgres keeps
// queueing notifications for a listener that will never consume them, and the
// gateway's reconnect-with-backoff loop adds another on every database blip.
//
// Readiness is established by DELIVERING a notification and waiting for
// onNotify, never by sleeping: a clock-based wait would measure the machine.
func TestListenNeverReturnsASubscribedConnectionToThePool(t *testing.T) {
	// startListener returns a store whose single pooled connection is
	// subscribed and proven live, plus a cancel and a done channel.
	startListener := func(t *testing.T, onNotify func(context.CancelFunc)) (*Store, context.CancelFunc, chan error) {
		t.Helper()
		s := listenTestStore(t)
		notifier := listenTestStore(t) // its own pool: s has no free connection
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		live := make(chan struct{}, 1)
		go func() {
			done <- s.Listen(ctx, EntitlementChannel, func(_ string) {
				select {
				case live <- struct{}{}:
				default:
				}
				if onNotify != nil {
					// cancel is captured once, at closure creation, and never
					// reassigned. An earlier version captured a variable the
					// test goroutine assigned afterwards, which -race caught
					// as a genuine data race between the two goroutines.
					onNotify(cancel)
				}
			})
		}()
		// Retry the NOTIFY until one lands: LISTEN races the goroutine start,
		// and a notification sent before LISTEN executes is simply not delivered.
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := notifier.pool.Exec(context.Background(),
				`SELECT pg_notify($1,$2)`, EntitlementChannel, "ready"); err != nil {
				t.Fatalf("notify: %v", err)
			}
			select {
			case <-live:
				return s, cancel, done
			case <-time.After(200 * time.Millisecond):
			}
		}
		t.Fatal("listener never received a notification, so it was never subscribed")
		return nil, nil, nil
	}

	t.Run("cancelled while blocked in WaitForNotification", func(t *testing.T) {
		s, cancel, done := startListener(t, nil)
		cancel()
		waitForListen(t, done)
		if got := poolListeningChannels(t, s); len(got) > 0 {
			t.Errorf("pool handed out a connection still subscribed to %v; "+
				"Listen must not return LISTEN state to the pool", got)
		}
	})

	t.Run("cancelled during onNotify, so WaitForNotification is never re-entered", func(t *testing.T) {
		// Cancelling from inside onNotify leaves the connection healthy and
		// idle at the instant Listen unwinds, so the next WaitForNotification
		// call returns on the already-done context without ever touching the
		// connection. That is a different exit path from the subtest above.
		s, _, done := startListener(t, func(cancel context.CancelFunc) { cancel() })
		waitForListen(t, done)
		if got := poolListeningChannels(t, s); len(got) > 0 {
			t.Errorf("pool handed out a connection still subscribed to %v; "+
				"Listen must not return LISTEN state to the pool", got)
		}
	})

	t.Run("the pool is still usable afterwards", func(t *testing.T) {
		s, cancel, done := startListener(t, nil)
		cancel()
		waitForListen(t, done)
		// Hijack removes the connection from the pool permanently, so this
		// asserts the pool replaces it rather than deadlocking at max_conns=1.
		var one int
		if err := s.pool.QueryRow(context.Background(), `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("pool unusable after Listen returned: %v", err)
		}
	})
}

func waitForListen(t *testing.T, done chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Listen did not return after its context was cancelled")
	}
}
