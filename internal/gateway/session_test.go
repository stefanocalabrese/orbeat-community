package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestUpstreamSession builds a real in-memory MCP client session so tests
// can observe close-propagation (a closed session returns ErrConnectionClosed).
func newTestUpstreamSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "unit-upstream", Version: "0"}, nil)
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "unit-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func assertUpstreamClosed(t *testing.T, cs *mcp.ClientSession) {
	t.Helper()
	if err := cs.Ping(context.Background(), nil); !errors.Is(err, mcp.ErrConnectionClosed) {
		t.Fatalf("upstream session must be closed, Ping err = %v", err)
	}
}

func TestSessionCacheGetOrBuildSingleFlight(t *testing.T) {
	c := newSessionCache(time.Minute, time.Hour, nil)
	defer c.closeAll()
	var builds int32
	const n = 20
	var wg sync.WaitGroup
	results := make([]*session, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := c.getOrBuild("subj", time.Now(), func() (*session, error) {
				atomic.AddInt32(&builds, 1)
				time.Sleep(20 * time.Millisecond) // widen the race window
				return &session{subject: "subj", slugToServer: map[string]string{}}, nil
			})
			if err != nil {
				t.Errorf("getOrBuild: %v", err)
				return
			}
			results[i] = s
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&builds); got != 1 {
		t.Fatalf("build ran %d times, want 1 (single-flight failed)", got)
	}
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatal("concurrent callers got different sessions")
		}
	}
}

func TestSessionCacheGetOrBuildCachesResult(t *testing.T) {
	c := newSessionCache(time.Minute, time.Hour, nil)
	defer c.closeAll()
	built := &session{subject: "s", slugToServer: map[string]string{}}
	s1, _ := c.getOrBuild("s", time.Now(), func() (*session, error) { return built, nil })
	s2, _ := c.getOrBuild("s", time.Now(), func() (*session, error) {
		t.Fatal("build should not run on a cache hit")
		return nil, nil
	})
	if s1 != built || s2 != built {
		t.Fatal("cached session not returned")
	}
}

func TestSessionCacheGetEviction(t *testing.T) {
	const (
		ttl    = time.Minute
		maxAge = 5 * time.Minute
	)
	base := time.Now()
	cases := []struct {
		name        string
		lastSeenAgo time.Duration
		builtAgo    time.Duration
		dirty       bool
		wantHit     bool
	}{
		{name: "fresh hit", lastSeenAgo: time.Second, builtAgo: time.Second, wantHit: true},
		{name: "idle-expired", lastSeenAgo: ttl + time.Second, builtAgo: ttl + time.Second, wantHit: false},
		{name: "max-age-expired despite recent lastSeen", lastSeenAgo: time.Second, builtAgo: maxAge + time.Second, wantHit: false},
		{name: "dirty", lastSeenAgo: time.Second, builtAgo: time.Second, dirty: true, wantHit: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newSessionCache(ttl, maxAge, nil)
			defer c.closeAll()
			s := &session{subject: "s", slugToServer: map[string]string{}}
			c.put("s", s, base.Add(-tc.builtAgo)) // stamps builtAt and lastSeen
			s.lastSeen = base.Add(-tc.lastSeenAgo)
			if tc.dirty {
				s.markDirty()
			}
			got, hit := c.get("s", base)
			if hit != tc.wantHit {
				t.Fatalf("get hit = %v, want %v", hit, tc.wantHit)
			}
			if tc.wantHit && got != s {
				t.Fatal("hit returned a different session")
			}
			if !tc.wantHit {
				c.mu.Lock()
				_, still := c.m["s"]
				c.mu.Unlock()
				if still {
					t.Fatal("expired session must be removed from the map")
				}
			}
		})
	}
}

func TestSessionCacheGetOrBuildRebuildsPastMaxAge(t *testing.T) {
	// ttl deliberately exceeds maxAge so only the max-age ceiling can trigger
	// the rebuild (idle eviction would mask it otherwise).
	c := newSessionCache(time.Hour, 5*time.Minute, nil)
	defer c.closeAll()
	old := &session{subject: "s", slugToServer: map[string]string{}}
	now := time.Now()
	c.put("s", old, now)

	rebuilt := &session{subject: "s", slugToServer: map[string]string{}}
	var builds int
	got, err := c.getOrBuild("s", now.Add(5*time.Minute+time.Second), func() (*session, error) {
		builds++
		return rebuilt, nil
	})
	if err != nil {
		t.Fatalf("getOrBuild: %v", err)
	}
	if builds != 1 {
		t.Fatalf("build ran %d times, want 1 (max-age must force a rebuild)", builds)
	}
	if got != rebuilt {
		t.Fatal("getOrBuild returned the stale session instead of the rebuilt one")
	}
}

func TestSessionCacheReapEvictsIdle(t *testing.T) {
	c := newSessionCache(time.Minute, time.Hour, nil)
	defer c.closeAll()
	base := time.Now()

	up := newTestUpstreamSession(t)
	idle := &session{subject: "idle", slugToServer: map[string]string{}, upstreams: []*upstreamConn{{session: up}}}
	fresh := &session{subject: "fresh", slugToServer: map[string]string{}}
	c.put("idle", idle, base.Add(-2*time.Minute))
	c.put("fresh", fresh, base)

	c.reap(base)

	c.mu.Lock()
	_, idleThere := c.m["idle"]
	_, freshThere := c.m["fresh"]
	c.mu.Unlock()
	if idleThere {
		t.Fatal("idle session must be reaped from the map")
	}
	if !freshThere {
		t.Fatal("fresh session must survive the reap")
	}
	assertUpstreamClosed(t, up)
}

func TestSessionCacheReaperEvictsWithoutGet(t *testing.T) {
	c := newSessionCache(20*time.Millisecond, time.Hour, nil)
	defer c.closeAll()
	c.put("s", &session{subject: "s", slugToServer: map[string]string{}}, time.Now())

	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.m)
		c.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("reaper never evicted the idle session (no get was issued)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSessionCacheReapAfterCloseAllIsNoop(t *testing.T) {
	c := newSessionCache(time.Minute, time.Hour, nil)
	c.closeAll()
	c.closeAll() // idempotent: must not double-close done
	c.reap(time.Now())
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) != 0 {
		t.Fatal("cache must stay empty after closeAll")
	}
}

func TestSessionCachePutAfterCloseAllClosesSession(t *testing.T) {
	c := newSessionCache(time.Minute, time.Hour, nil)
	c.closeAll()

	up := newTestUpstreamSession(t)
	late := &session{subject: "late", slugToServer: map[string]string{}, upstreams: []*upstreamConn{{session: up}}}
	c.put("late", late, time.Now())

	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n != 0 {
		t.Fatal("put on a closed cache must not insert (shutdown race would leak the session)")
	}
	assertUpstreamClosed(t, up)
}

// boundLen reads the size of the Mcp-Session-Id binding index under the cache
// mutex, so a test can assert on it without racing the background reaper.
func boundLen(c *sessionCache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bound)
}

// TestSessionCacheBindingIndexIsBoundedOnEveryEvictionPath proves the A1
// Mcp-Session-Id -> *session index does not grow without bound, and that it is
// EVERY eviction path that keeps it that way.
//
// The index is what makes withSession answer a stale transport session with a
// 404 instead of serving it from a frozen *mcp.Server. Its entries outlive the
// sessions they name -- an eviction TOMBSTONES an id rather than deleting it,
// so that the 404 can still say WHY the session went away. (Until 2026-08-30
// that was a safety property too: a deleted id was an UNKNOWN id, and
// withSession adopted unknown ids. It no longer does, so a deleted id is now
// refused as "unbound" rather than served -- the tombstone buys the reason,
// not the refusal.) Something still has to expire them, and nothing gated that
// before this test: no test in this package referenced bindTransport,
// staleTransportsLocked or sweepTransportsLocked at all.
//
// Each case asserts three things, and they fail for different reasons:
//
//   - the eviction TOMBSTONED (did not delete) every id, so the 404 survives;
//   - the tombstone carries THAT path's reclaim cause, which is the value
//     withSession echoes in X-Orbeat-Session-Rebuilt. This is the assertion
//     that covers reap's own staleTransportsLocked call: reap sweeps in the
//     same pass, so a missing tombstone there is silently repaired as
//     reclaimUnknown and the size assertions below stay green while the
//     operator-facing reason is lost;
//   - ONE sweep past tombstoneHorizon empties the index. One, not "eventually":
//     an eviction path that forgets to tombstone leaves a LIVE binding, which
//     sweepTransportsLocked can only tombstone (at sweep time), never delete,
//     so it would take a second horizon to clear and this assertion goes red.
//
// The fifth call site, put's post-close race, is deliberately not covered:
// bindTransport refuses to bind while the cache is closed and closeAll has
// already tombstoned everything that existed, so no binding can reach it. Its
// own comment says as much.
func TestSessionCacheBindingIndexIsBoundedOnEveryEvictionPath(t *testing.T) {
	// Production-sized so the background reaper (ticking at ttl/2) cannot fire
	// during the test: every sweep here is driven explicitly with an
	// argument-supplied now.
	const (
		ttl    = 10 * time.Minute
		maxAge = 5 * time.Minute
		n      = 8
	)
	cases := []struct {
		name      string
		wantCause string
		// evict removes s from the cache and returns the instant the eviction
		// stamped on its tombstones.
		evict func(t *testing.T, c *sessionCache, s *session, base time.Time) time.Time
	}{
		{
			name:      "get past maxAge",
			wantCause: reclaimMaxAge,
			evict: func(t *testing.T, c *sessionCache, s *session, base time.Time) time.Time {
				at := base.Add(maxAge + time.Second)
				if _, ok := c.get(s.subject, at); ok {
					t.Fatal("get past maxAge must miss and evict")
				}
				return at
			},
		},
		{
			name:      "reap",
			wantCause: reclaimMaxAge,
			evict: func(t *testing.T, c *sessionCache, s *session, base time.Time) time.Time {
				at := base.Add(maxAge + time.Second)
				c.reap(at)
				return at
			},
		},
		{
			name:      "invalidateTenant",
			wantCause: reclaimEntitlementChange,
			evict: func(t *testing.T, c *sessionCache, s *session, base time.Time) time.Time {
				at := time.Now() // invalidateTenant stamps staleAt itself.
				if got := c.invalidateTenant(s.tenantID); got != 1 {
					t.Fatalf("invalidateTenant dropped %d sessions, want 1", got)
				}
				return at
			},
		},
		{
			name:      "closeAll",
			wantCause: reclaimExplicit,
			evict: func(t *testing.T, c *sessionCache, s *session, base time.Time) time.Time {
				at := time.Now() // closeAll stamps staleAt itself.
				c.closeAll()
				return at
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newSessionCache(ttl, maxAge, nil)
			defer c.closeAll() // idempotent; harmless after the closeAll case.
			base := time.Now()
			s := &session{subject: "s", tenantID: "tenant-1", slugToServer: map[string]string{}}
			c.put("s", s, base)

			ids := make([]string, n)
			for i := range ids {
				ids[i] = fmt.Sprintf("mcp-session-%d", i)
				c.bindTransport(ids[i], s)
			}
			if got := boundLen(c); got != n {
				t.Fatalf("bound = %d after binding %d ids, want %d", got, n, n)
			}

			staleAt := tc.evict(t, c, s, base)

			if got := boundLen(c); got != n {
				t.Fatalf("bound = %d immediately after the eviction, want %d: the ids must be TOMBSTONED, not deleted, or the next request replaying one is refused as a bare \"unbound\" and neither the log line nor %s can tell an operator that %s is what happened", got, n, sessionRebuiltHeader, tc.wantCause)
			}
			for _, id := range ids {
				b := c.lookupTransport(id)
				switch {
				case !b.known:
					t.Fatalf("%s is unknown after the eviction, want a tombstone", id)
				case b.sess != nil:
					t.Fatalf("%s still points at a session after the eviction; the tombstone must release it", id)
				case b.cause != tc.wantCause:
					t.Fatalf("%s tombstone cause = %q, want %q: this is the value withSession echoes in %s, so a 404 written from it cannot tell an operator why", id, b.cause, tc.wantCause, sessionRebuiltHeader)
				}
			}

			// 2*maxAge spelled out rather than read back from
			// c.tombstoneHorizon(): a sweep scheduled off the function under
			// test moves with it and passes on any horizon, however large.
			c.reap(staleAt.Add(2*maxAge + time.Second))
			if got := boundLen(c); got != 0 {
				t.Fatalf("bound = %d after one sweep past 2*maxAge (%v), want 0: the binding index grows without bound on this eviction path", got, 2*maxAge)
			}
		})
	}
}

// TestSessionCacheTombstoneSurvivesUntilTheHorizon is the other direction of
// the same gate: a sweep that is EARLY must not forget the tombstone. Without
// this, boundedness alone is satisfied by a sweep that deletes everything on
// sight.
//
// WHAT IT DEFENDS CHANGED ON 2026-08-30, and the assertion is unchanged
// because the value is. It used to defend a safety property -- forgetting a
// tombstone handed the id back to withSession's never-seen branch, which
// rebound it to the current session and let it reach the frozen transport.
// That branch is gone; withSession refuses an id it holds no binding for. What
// the horizon defends now is the 404's REASON: while the SDK may still be
// holding the transport session behind a tombstone, a client replaying its id
// should be told "max_age", not a bare "unbound". The SDK's own idle timeout
// is sessionMaxAge and it PAUSES for the duration of an in-flight POST, which
// is why maxAge exactly is not enough and 2*maxAge is the value. See
// tombstoneHorizon for the full argument, including why shortening it is a
// trade against operators rather than a correctness change.
//
// It pins the horizon VALUE, not just the inequality. The first draft reaped
// at staleAt+c.tombstoneHorizon() and was therefore unfalsifiable: reverting
// tombstoneHorizon to the old c.maxAge moved the sweep along with it and left
// this test green (measured 2026-08-29). The 2*maxAge below is spelled out
// from the test's own constants for that reason.
func TestSessionCacheTombstoneSurvivesUntilTheHorizon(t *testing.T) {
	const (
		ttl    = 10 * time.Minute
		maxAge = 5 * time.Minute
	)
	c := newSessionCache(ttl, maxAge, nil)
	defer c.closeAll()

	base := time.Now()
	s := &session{subject: "s", tenantID: "tenant-1", slugToServer: map[string]string{}}
	c.put("s", s, base)
	c.bindTransport("mcp-session-0", s)

	staleAt := base.Add(maxAge + time.Second)
	if _, ok := c.get("s", staleAt); ok {
		t.Fatal("get past maxAge must miss and evict")
	}

	if got, want := c.tombstoneHorizon(), 2*maxAge; got != want {
		t.Fatalf("tombstoneHorizon = %v, want %v (2*maxAge): maxAge alone leaves a window in which the gateway has forgotten WHY it evicted a session while the SDK may still hold the transport session behind it, because the SDK's idle timer is PAUSED for the whole duration of an in-flight POST and only reset when it ends -- the replay is still refused, but as a bare %q", got, want, sessionUnbound)
	}

	// Exactly at the horizon, which sweepTransportsLocked treats as not yet
	// expired: still a tombstone, still 404-ing.
	c.reap(staleAt.Add(2 * maxAge))
	if b := c.lookupTransport("mcp-session-0"); !b.known {
		t.Fatalf("tombstone was forgotten at staleAt+2*maxAge (%v); it must outlive the SDK's own reclamation of the transport session behind it, so a client replaying the id while that transport can still exist is told why", 2*maxAge)
	}
}
