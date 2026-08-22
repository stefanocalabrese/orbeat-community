package ratelimit

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

// Refill is deterministic because now is a parameter, not time.Now().
func TestAllowAtBurstThenRefill(t *testing.T) {
	l := New(10, 5, time.Minute, 100)
	defer l.Close()

	for i := 0; i < 5; i++ {
		if ok, _ := l.AllowAt("k", t0); !ok {
			t.Fatalf("request %d within burst was rejected", i)
		}
	}
	if ok, retry := l.AllowAt("k", t0); ok {
		t.Fatal("6th request at burst 5 was allowed")
	} else if retry <= 0 {
		t.Errorf("retryAfter = %v, want > 0", retry)
	}
	// One token accrues after 1/rps = 100ms.
	if ok, _ := l.AllowAt("k", t0.Add(100*time.Millisecond)); !ok {
		t.Error("request after one refill interval was rejected")
	}
}

// A rejected request must NOT consume or postpone capacity. Computing
// retryAfter via Reserve() would charge a token for the rejection, so a client
// hammering faster than the limit would push its own recovery out forever.
func TestRejectedRequestsDoNotConsumeTokens(t *testing.T) {
	l := New(10, 1, time.Minute, 100)
	defer l.Close()

	if ok, _ := l.AllowAt("k", t0); !ok {
		t.Fatal("first request rejected")
	}
	for i := 0; i < 50; i++ {
		if ok, _ := l.AllowAt("k", t0); ok {
			t.Fatalf("request %d should have been rejected", i)
		}
	}
	// Exactly one refill interval after t0, one token exists — the 50
	// rejections must not have pushed this out.
	if ok, retry := l.AllowAt("k", t0.Add(100*time.Millisecond)); !ok {
		t.Fatalf("50 rejections postponed recovery: still rejected, retryAfter=%v", retry)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(10, 1, time.Minute, 100)
	defer l.Close()

	if ok, _ := l.AllowAt("a", t0); !ok {
		t.Fatal("a: first rejected")
	}
	if ok, _ := l.AllowAt("a", t0); ok {
		t.Fatal("a: second allowed")
	}
	if ok, _ := l.AllowAt("b", t0); !ok {
		t.Fatal("b starved by a's bucket")
	}
}

// Eviction is otherwise unobservable, so Len() exists for this test.
func TestSweepEvictsIdleKeys(t *testing.T) {
	l := New(10, 5, time.Minute, 100)
	defer l.Close()

	l.AllowAt("k", t0)
	if got := l.Len(); got != 1 {
		t.Fatalf("Len after one key = %d, want 1", got)
	}
	if n := l.sweep(t0.Add(30 * time.Second)); n != 0 {
		t.Errorf("swept %d entries before the TTL elapsed, want 0", n)
	}
	if n := l.sweep(t0.Add(2 * time.Minute)); n != 1 {
		t.Errorf("swept %d entries after the TTL, want 1", n)
	}
	if got := l.Len(); got != 0 {
		t.Errorf("Len after sweep = %d, want 0", got)
	}
}

// Spec §3.1 invariant 1. If lastUsed were stamped only on allowed calls, a
// throttled client would look idle by construction, get swept, and come back
// with a full bucket — the limiter resetting for exactly the clients it is
// throttling.
//
// rps must be low enough that the loop below never re-earns a token: a
// REJECTED AllowN call does not advance x/time/rate's internal clock (its
// reserveN only updates lim.last/tokens `if ok`), so tokens accrue purely
// from elapsed time since the one successful drain at t0. rps=1 (one token
// per second) with calls spaced exactly one second apart — the value
// verbatim in the plan this test was drafted from — refills exactly in step
// with the loop, so every call is ALLOWED and the test passes identically
// whether or not lastUsed is stamped on rejection: a red-proof (moving the
// lastUsed stamp inside the AllowN-success branch) confirmed this left the
// whole suite green. rps=0.01 keeps 20s of elapsed time at 0.2 tokens, well
// under the 1 needed, so every loop call is a genuine rejection.
func TestThrottledKeyIsNotSweptWhileBeingRejected(t *testing.T) {
	l := New(0.01, 1, 10*time.Second, 100)
	defer l.Close()

	l.AllowAt("k", t0) // drains the bucket
	// Keep getting rejected across a span longer than the TTL.
	for i := 1; i <= 20; i++ {
		if ok, _ := l.AllowAt("k", t0.Add(time.Duration(i)*time.Second)); ok {
			t.Fatalf("call %d was allowed — rps too high for this test's premise (all loop calls must be rejected)", i)
		}
	}
	if n := l.sweep(t0.Add(21 * time.Second)); n != 0 {
		t.Fatalf("swept %d entries for a key rejected 1s ago — lastUsed is not stamped on rejections", n)
	}
}

// Spec §3.1: at the cap, evict LRU. Never reject a new key, never bypass.
func TestAtCapEvictsLRUAndKeepsLimiting(t *testing.T) {
	l := New(10, 1, time.Hour, 2)
	defer l.Close()

	l.AllowAt("old", t0)
	l.AllowAt("mid", t0.Add(time.Second))
	// "new" exceeds maxEntries=2 and must still be ALLOWED (not rejected at cap).
	if ok, _ := l.AllowAt("new", t0.Add(2*time.Second)); !ok {
		t.Fatal("a brand-new key was rejected at the cap")
	}
	if got := l.Len(); got > 2 {
		t.Errorf("Len = %d, want <= maxEntries 2", got)
	}
	// And the limiter must still limit that new key — no bypass at the cap.
	if ok, _ := l.AllowAt("new", t0.Add(2*time.Second)); ok {
		t.Error("a hammering new key was not throttled at the cap")
	}
}
