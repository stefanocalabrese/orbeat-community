package ratelimit

import (
	"sync"
	"testing"
	"time"
)

func TestConcurrencyCapAdmitsUpToLimitThenRejects(t *testing.T) {
	c := NewConcurrency(2, time.Minute)
	defer c.Close()

	r1, ok1 := c.Acquire("alice")
	r2, ok2 := c.Acquire("alice")
	if !ok1 || !ok2 {
		t.Fatalf("first two acquires must succeed, got %v %v", ok1, ok2)
	}
	if _, ok3 := c.Acquire("alice"); ok3 {
		t.Fatal("third concurrent acquire must be rejected at a cap of 2")
	}
	r1()
	if _, ok := c.Acquire("alice"); !ok {
		t.Fatal("a slot released must be immediately reusable")
	}
	r2()
}

// TestConcurrencyCapReturnsToZero is the LEAK gate, and it asserts the COUNT
// rather than "one more call succeeded": a leak of one slot out of N passes the
// weaker assertion while the cap silently shrinks on every burst.
func TestConcurrencyCapReturnsToZero(t *testing.T) {
	c := NewConcurrency(3, time.Minute)
	defer c.Close()

	var releases []func()
	for i := 0; i < 3; i++ {
		r, ok := c.Acquire("alice")
		if !ok {
			t.Fatalf("acquire %d rejected below the cap", i)
		}
		releases = append(releases, r)
	}
	for _, r := range releases {
		r()
	}
	if n := c.InFlight("alice"); n != 0 {
		t.Fatalf("in-flight = %d after releasing everything, want 0 — a slot leaked", n)
	}
}

// TestConcurrencyReleaseIsIdempotent guards a double release from driving the
// count negative, which would silently RAISE the effective cap for that key.
func TestConcurrencyReleaseIsIdempotent(t *testing.T) {
	c := NewConcurrency(1, time.Minute)
	defer c.Close()

	r, _ := c.Acquire("alice")
	r()
	r()
	if n := c.InFlight("alice"); n != 0 {
		t.Fatalf("in-flight = %d after a double release, want 0", n)
	}
	if _, ok := c.Acquire("alice"); !ok {
		t.Fatal("cap of 1 must admit one call after release")
	}
	if _, ok := c.Acquire("alice"); ok {
		t.Fatal("double release raised the effective cap")
	}
}

func TestConcurrencyIsolatesPrincipals(t *testing.T) {
	c := NewConcurrency(1, time.Minute)
	defer c.Close()

	if _, ok := c.Acquire("alice"); !ok {
		t.Fatal("alice's first acquire must succeed")
	}
	if _, ok := c.Acquire("bob"); !ok {
		t.Fatal("bob must not be affected by alice being at her cap")
	}
}

// TestConcurrencySweepNeverDropsLiveEntries is the eviction gate (spec §6).
func TestConcurrencySweepNeverDropsLiveEntries(t *testing.T) {
	c := NewConcurrency(2, time.Nanosecond) // everything is instantly "old"
	defer c.Close()

	r, ok := c.Acquire("alice")
	if !ok {
		t.Fatal("acquire failed")
	}
	time.Sleep(2 * time.Millisecond)
	c.sweep(time.Now())

	if n := c.InFlight("alice"); n != 1 {
		t.Fatalf("in-flight = %d after a sweep with a live call, want 1 — the sweep dropped a live entry", n)
	}
	if _, ok := c.Acquire("alice"); !ok {
		t.Fatal("second slot should still be available")
	}
	if _, ok := c.Acquire("alice"); ok {
		t.Fatal("cap stopped applying after a sweep — the live count was lost")
	}
	r()
}

// TestConcurrencySweepEvictsIdleEntries is the converse: zero-count entries MUST
// age out, or the map grows for every principal ever seen.
func TestConcurrencySweepEvictsIdleEntries(t *testing.T) {
	c := NewConcurrency(2, time.Nanosecond)
	defer c.Close()

	r, _ := c.Acquire("alice")
	r()
	time.Sleep(2 * time.Millisecond)
	c.sweep(time.Now())

	if n := c.Len(); n != 0 {
		t.Fatalf("Len = %d after sweeping an idle entry, want 0", n)
	}
}

func TestConcurrencyUnlimitedWhenMaxNonPositive(t *testing.T) {
	c := NewConcurrency(0, time.Minute)
	defer c.Close()
	for i := 0; i < 50; i++ {
		if _, ok := c.Acquire("alice"); !ok {
			t.Fatalf("max <= 0 must disable the cap; rejected at %d", i)
		}
	}
}

func TestConcurrencyIsRaceFree(t *testing.T) {
	c := NewConcurrency(100, time.Minute)
	defer c.Close()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, ok := c.Acquire("alice"); ok {
				r()
			}
		}()
	}
	wg.Wait()
	if n := c.InFlight("alice"); n != 0 {
		t.Fatalf("in-flight = %d after concurrent acquire/release, want 0", n)
	}
}
