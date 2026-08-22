package gateway

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
)

// Reclaim causes for the orbeat.gateway.sessions.reclaimed counter (gateway
// session lifecycle design, 2026-08-16 §5) — a small closed set. The
// per-subject cache key is deliberately never attached as an attribute here
// (same rule the rate-limiting slice applied to its own counters: an
// unbounded label value would make the counter itself an unbounded-
// cardinality time series).
const (
	reclaimDirty    = "dirty"          // markDirty(): a proxied call hit a closed upstream.
	reclaimMaxAge   = "max_age"        // builtAt older than maxAge, regardless of activity.
	reclaimIdle     = "idle_timeout"   // lastSeen older than ttl.
	reclaimExplicit = "explicit_close" // closeAll() (graceful shutdown) or a post-close put race.
)

// Results for the orbeat.gateway.sessions.lookup counter (gateway
// upstream-connect and session-cache metrics design, 2026-08-18 §2/§4).
const (
	lookupHit  = "hit"
	lookupMiss = "miss"
)

// session is one caller's (subject's) assembled gateway state.
type session struct {
	subject      string
	tenantID     string
	actor        string
	entitlements []store.Entitlement
	slugToServer map[string]string
	upstreams    []*upstreamConn
	mcpServer    *mcp.Server
	lastSeen     time.Time
	builtAt      time.Time
	// dirty flags the session for eviction: set when a proxied call hits a
	// closed upstream connection. Atomic because proxy closures set it
	// concurrently with cache reads.
	dirty atomic.Bool
}

// markDirty flags the session so the cache treats it as a miss (evict + close
// + rebuild on the next request).
func (s *session) markDirty() { s.dirty.Store(true) }

func (s *session) close() {
	for _, u := range s.upstreams {
		_ = u.session.Close()
		if u.transport != nil {
			u.transport.CloseIdleConnections()
		}
		if u.cancel != nil {
			// Sever the connection-scoped context (e.g. the SSE hanging GET),
			// tying upstream stream lifetime to session lifetime.
			u.cancel()
		}
	}
}

// sessionCache is a subject-keyed cache with idle (ttl) and max-age eviction.
type sessionCache struct {
	mu     sync.Mutex
	m      map[string]*session
	ttl    time.Duration
	maxAge time.Duration
	closed bool
	done   chan struct{}
	group  singleflight.Group
	// metrics is nil-safe (mirrors ratelimit.Observability's pattern): every
	// existing caller that builds a sessionCache without wiring telemetry
	// (most tests) behaves exactly as before this counter existed.
	metrics *telemetry.Metrics
}

// newSessionCache builds a cache evicting sessions idle for > ttl or older
// than maxAge (a revocation-staleness ceiling regardless of activity).
// A background reaper releases expired sessions even when their subject never
// issues another request; closeAll stops it. ttl must be > 0 (the reaper
// ticks at ttl/2, and time.NewTicker panics on a non-positive interval).
// metrics may be nil (disables the sessions-reclaimed counter; the cache
// itself is unaffected).
func newSessionCache(ttl, maxAge time.Duration, metrics *telemetry.Metrics) *sessionCache {
	c := &sessionCache{m: make(map[string]*session), ttl: ttl, maxAge: maxAge, done: make(chan struct{}), metrics: metrics}
	go func() {
		ticker := time.NewTicker(ttl / 2)
		defer ticker.Stop()
		for {
			select {
			case <-c.done:
				return
			case now := <-ticker.C:
				c.reap(now)
			}
		}
	}()
	return c
}

// getOrBuild returns the cached session for subject, or builds exactly one even
// under concurrent callers (single-flight), caching the result. Concurrent
// callers for the same subject share the single build, preventing duplicate
// upstream connections and the orphaned-session leak that a plain
// get/build/put would cause.
func (c *sessionCache) getOrBuild(subject string, now time.Time, build func() (*session, error)) (*session, error) {
	if s, ok := c.get(subject, now); ok {
		c.recordLookup(lookupHit)
		return s, nil
	}
	c.recordLookup(lookupMiss)
	v, err, _ := c.group.Do(subject, func() (any, error) {
		// Re-check under the flight: a prior flight for this subject may have
		// already populated the cache.
		if s, ok := c.get(subject, time.Now()); ok {
			return s, nil
		}
		s, err := build()
		if err != nil {
			return nil, err
		}
		c.put(s, time.Now())
		return s, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*session), nil
}

func (c *sessionCache) get(subject string, now time.Time) (*session, bool) {
	c.mu.Lock()
	s, ok := c.m[subject]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	if cause, expired := c.reclaimCause(s, now); expired {
		delete(c.m, subject)
		c.mu.Unlock()
		// close() does network I/O (HTTP DELETE per upstream); off the mutex so
		// one hung upstream cannot head-of-line-block every other subject.
		s.close()
		c.recordReclaim(cause)
		return nil, false
	}
	s.lastSeen = now
	c.mu.Unlock()
	return s, true
}

// reclaimCause reports whether s must be evicted and, if so, which rule
// fired: flagged dirty (dead upstream), past the max-age ceiling regardless
// of activity, or idle past the ttl — checked in that order, matching this
// cache's historical OR precedence, so a session matching more than one rule
// at once is attributed to the first (most specific) one. Callers hold c.mu.
func (c *sessionCache) reclaimCause(s *session, now time.Time) (cause string, expired bool) {
	switch {
	case s.dirty.Load():
		return reclaimDirty, true
	case now.Sub(s.builtAt) > c.maxAge:
		return reclaimMaxAge, true
	case now.Sub(s.lastSeen) > c.ttl:
		return reclaimIdle, true
	default:
		return "", false
	}
}

// recordReclaim increments the sessions-reclaimed counter for cause. Never
// called under c.mu (metric export must not block cache access).
func (c *sessionCache) recordReclaim(cause string) {
	if c.metrics == nil || c.metrics.SessionsReclaimed == nil {
		return
	}
	c.metrics.SessionsReclaimed.Add(context.Background(), 1, metric.WithAttributes(attribute.String("cause", cause)))
}

// recordLookup increments the sessions-lookup counter for result (lookupHit
// or lookupMiss). Called exactly once per getOrBuild call, off the
// optimistic get at the top of getOrBuild — never off get() itself, which
// getOrBuild calls a second time inside the singleflight on a miss and would
// double-count (gateway upstream-connect and session-cache metrics design,
// 2026-08-18 §4).
func (c *sessionCache) recordLookup(result string) {
	if c.metrics == nil || c.metrics.SessionLookup == nil {
		return
	}
	c.metrics.SessionLookup.Add(context.Background(), 1, metric.WithAttributes(attribute.String("result", result)))
}

// size returns the number of subjects currently cached — the live-session
// gauge (telemetry.RegisterSessionGauge) reads this at collection time.
func (c *sessionCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func (c *sessionCache) put(s *session, now time.Time) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		// Shutdown race: a build finishing after closeAll must not land in a
		// cache nobody will drain again — close it (off the mutex) instead of
		// leaking it.
		s.close()
		c.recordReclaim(reclaimExplicit)
		return
	}
	s.lastSeen = now
	s.builtAt = now
	c.m[s.subject] = s
	c.mu.Unlock()
}

// reap sweeps the cache, closing and removing every session get would treat as
// expired. Called by the background reaper so idle sessions are released even
// when their subject never returns. Victims are collected under the mutex but
// closed outside it (network I/O).
func (c *sessionCache) reap(now time.Time) {
	c.mu.Lock()
	type victim struct {
		s     *session
		cause string
	}
	var victims []victim
	for subject, s := range c.m {
		if cause, expired := c.reclaimCause(s, now); expired {
			delete(c.m, subject)
			victims = append(victims, victim{s, cause})
		}
	}
	c.mu.Unlock()
	for _, v := range victims {
		v.s.close()
		c.recordReclaim(v.cause)
	}
}

// closeAll tears down every cached session's upstream connections, empties the
// cache, marks it closed (a late put then closes its session instead of
// inserting), and stops the reaper. It is idempotent and used for graceful
// gateway shutdown. Sessions are removed under the mutex and closed outside it.
func (c *sessionCache) closeAll() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	victims := make([]*session, 0, len(c.m))
	for subject, s := range c.m {
		victims = append(victims, s)
		delete(c.m, subject)
	}
	c.mu.Unlock()
	for _, s := range victims {
		s.close()
		// closeAll empties c.m on its first call, so a second (idempotent)
		// call finds nothing here and this loop naturally records nothing —
		// no separate "already closed" bookkeeping needed to avoid
		// double-counting.
		c.recordReclaim(reclaimExplicit)
	}
}
