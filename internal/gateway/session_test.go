package gateway

import (
	"context"
	"errors"
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
			c.put(s, base.Add(-tc.builtAgo)) // stamps builtAt and lastSeen
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
	c.put(old, now)

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
	c.put(idle, base.Add(-2*time.Minute))
	c.put(fresh, base)

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
	c.put(&session{subject: "s", slugToServer: map[string]string{}}, time.Now())

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
	c.put(late, time.Now())

	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n != 0 {
		t.Fatal("put on a closed cache must not insert (shutdown race would leak the session)")
	}
	assertUpstreamClosed(t, up)
}
