package ratelimit

import (
	"testing"
	"time"
)

// TestConcurrencyDoubleReleaseWithOtherSlotsHeld isolates what sync.Once
// actually protects, which TestConcurrencyReleaseIsIdempotent cannot see.
//
// That test uses a cap of 1 and a single acquire, so the second release finds
// n == 0 and release()'s own guard absorbs it — meaning the test passes whether
// sync.Once is present or not, and dropping sync.Once leaves the whole package
// green. Measured, not assumed.
//
// The case only sync.Once can catch is a double release while OTHER slots are
// still held: with two acquires outstanding, releasing the first one twice sees
// n go 2 -> 1 -> 0, so the `n == 0` guard never fires. The count then reports
// zero while a call is genuinely in flight, which does not merely lose accuracy
// — it silently RAISES the effective cap for that principal, the opposite of
// what this limiter exists to do.
func TestConcurrencyDoubleReleaseWithOtherSlotsHeld(t *testing.T) {
	c := NewConcurrency(3, time.Minute)
	defer c.Close()

	releaseA, okA := c.Acquire("alice")
	_, okB := c.Acquire("alice") // slot B stays held for the whole test
	if !okA || !okB {
		t.Fatalf("both acquires must succeed below a cap of 3, got %v %v", okA, okB)
	}

	releaseA()
	releaseA() // the double release, while B is still in flight

	if n := c.InFlight("alice"); n != 1 {
		t.Fatalf("in-flight = %d, want 1 (slot B is still held) — the count was corrupted, which raises the effective cap", n)
	}
	// The cap must still bind at its real value: two more admits, then refusal.
	if _, ok := c.Acquire("alice"); !ok {
		t.Fatal("a third slot should be available (1 held, cap 3)")
	}
	if _, ok := c.Acquire("alice"); !ok {
		t.Fatal("a fourth slot should be available (2 held, cap 3)")
	}
	if _, ok := c.Acquire("alice"); ok {
		t.Fatal("cap of 3 admitted a fourth concurrent call — the count is under-reporting")
	}
}
