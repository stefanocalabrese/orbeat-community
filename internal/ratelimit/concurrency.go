package ratelimit

import (
	"sync"
	"time"
)

// slot tracks one key's in-flight count. idleSince is set when the count
// reaches zero and is the only thing the sweeper may evict on: an entry with
// n > 0 must NEVER be dropped (see sweep).
type slot struct {
	n         int
	idleSince time.Time
}

// ConcurrencyLimiter caps how many operations a key may have IN FLIGHT at once.
// It is deliberately not a Limiter: a token bucket bounds throughput over time,
// this bounds simultaneous occupancy, and the two failure modes differ enough
// that sharing an implementation would be misleading.
//
// max <= 0 disables the cap entirely (Acquire always succeeds), the same
// sentinel style Limiter uses for rps <= 0.
type ConcurrencyLimiter struct {
	mu       sync.Mutex
	slots    map[string]*slot
	max      int
	ttl      time.Duration
	stop     chan struct{}
	stopOnce sync.Once
}

// NewConcurrency returns a ConcurrencyLimiter and starts its idle-entry sweeper.
// Callers must Close it.
//
// There is deliberately no maxEntries ceiling, unlike Limiter. The number of
// entries with a live count is inherently bounded by the number of operations
// in flight across the process, and idle entries age out on ttl — so the map
// cannot grow without bound, and no eviction policy has to choose between
// bounding memory and preserving a live count.
func NewConcurrency(max int, ttl time.Duration) *ConcurrencyLimiter {
	c := &ConcurrencyLimiter{
		slots: make(map[string]*slot),
		max:   max,
		ttl:   ttl,
		stop:  make(chan struct{}),
	}
	go c.run()
	return c
}

func (c *ConcurrencyLimiter) run() {
	if c.ttl <= 0 {
		return
	}
	t := time.NewTicker(c.ttl)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case now := <-t.C:
			c.sweep(now)
		}
	}
}

// Close stops the sweeper. Safe to call more than once.
func (c *ConcurrencyLimiter) Close() error {
	c.stopOnce.Do(func() { close(c.stop) })
	return nil
}

// Acquire takes a slot for key. The returned release MUST be called exactly
// once per successful acquire — callers defer it immediately. It is idempotent:
// a double release would otherwise drive the count negative and silently RAISE
// the effective cap for that key.
//
// When ok is false no slot was taken and release is a no-op, so a caller that
// defers unconditionally is still correct.
func (c *ConcurrencyLimiter) Acquire(key string) (release func(), ok bool) {
	if c.max <= 0 {
		return func() {}, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.slots[key]
	if s == nil {
		s = &slot{}
		c.slots[key] = s
	}
	if s.n >= c.max {
		return func() {}, false
	}
	s.n++
	var once sync.Once
	return func() { once.Do(func() { c.release(key) }) }, true
}

func (c *ConcurrencyLimiter) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.slots[key]
	if s == nil || s.n == 0 {
		return
	}
	s.n--
	if s.n == 0 {
		s.idleSince = time.Now()
	}
}

// InFlight reports the current count for key. Exported for tests and for the
// rejection payload, which reports what the caller actually hit.
func (c *ConcurrencyLimiter) InFlight(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.slots[key]; s != nil {
		return s.n
	}
	return 0
}

// Max reports the configured cap, for the rejection payload.
func (c *ConcurrencyLimiter) Max() int { return c.max }

// Len reports the number of tracked keys.
func (c *ConcurrencyLimiter) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.slots)
}

// sweep drops entries that have been idle for longer than ttl.
//
// An entry with n > 0 is NEVER evicted, and that is the whole rule. Evicting a
// live entry loses its count, so the key's next Acquire starts from zero and the
// cap silently stops applying — to the busiest keys first, which are exactly the
// ones it exists to bound. A sweep copied from Limiter's would do precisely that.
func (c *ConcurrencyLimiter) sweep(now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for k, s := range c.slots {
		if s.n > 0 {
			continue
		}
		if now.Sub(s.idleSince) >= c.ttl {
			delete(c.slots, k)
			removed++
		}
	}
	return removed
}
