package ratelimit

import (
	"sync"
	"time"
)

// slot tracks one key's in-flight count. idleSince is set when the count
// reaches zero and is the only thing the sweeper may evict on: an entry with
// n > 0 must NEVER be dropped (see sweep).
//
// lastLogged is the concurrency cap's half of the shared log sampler (see
// sample.go), zero if this key has never produced a line. It is safe to hang
// off the slot for the same reason the rate limiter hangs its own off the
// bucket, and safer here: a rejection means n >= max > 0, so the entry the
// value lives on is one the sweeper is forbidden to evict.
type slot struct {
	n          int
	idleSince  time.Time
	lastLogged time.Time
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
//
// It does not participate in sampling at all, and that is the point rather
// than an omission. Every production call site takes a rejection log line, so
// they all use AcquireSampled; this one remains for tests and for any caller
// that reports nothing.
//
// It used to delegate to AcquireSampled and throw the third return away, which
// is not the same thing: the sampler stamps lastLogged when it DECIDES to log,
// so a caller that discarded the decision was silently spending the budget.
// One Acquire on a capped key would then keep the next real AcquireSampled
// quiet for a whole logSampleInterval, and the line that went missing is a
// rejection nobody would ever learn about. A non-reporting caller must be
// invisible to a reporting one.
func (c *ConcurrencyLimiter) Acquire(key string) (release func(), ok bool) {
	release, ok, _ = c.acquireAt(key, time.Now(), false)
	return release, ok
}

// AcquireSampled is AcquireAtSampled(key, time.Now()).
func (c *ConcurrencyLimiter) AcquireSampled(key string) (func(), bool, bool) {
	return c.AcquireAtSampled(key, time.Now())
}

// AcquireAtSampled is Acquire plus a third return, logRejection: true only for
// the first rejection for key within logSampleInterval of the last one that
// logged. It is the concurrency cap's counterpart to
// Limiter.AllowAtSampled, and it exists because MCPConcurrency used to pass a
// literal true, so every capped call warned while the doc beside it said the
// line was sampled.
//
// The cap needs this at least as much as the token bucket does: it sits behind
// the tools/call rate limiter in the gateway's middleware chain, so a single
// principal can drive it at the full admitted rate, 20/s on the shipped
// defaults, or 1200 warn lines a minute from one key.
//
// now is a parameter for the same reason it is on AllowAt: it is the only way
// to test interval behaviour without sleeping.
func (c *ConcurrencyLimiter) AcquireAtSampled(key string, now time.Time) (release func(), ok bool, logRejection bool) {
	return c.acquireAt(key, now, true)
}

// acquireAt is the single locked decision both Acquire and AcquireAtSampled
// delegate to, so their admit/reject outcomes can never drift apart.
//
// sample is what separates them. When it is false the sampler is neither read
// nor stamped, so a caller that will not log costs a later caller nothing; see
// Acquire's comment for the line that used to go missing.
func (c *ConcurrencyLimiter) acquireAt(key string, now time.Time, sample bool) (release func(), ok bool, logRejection bool) {
	if c.max <= 0 {
		return func() {}, true, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.slots[key]
	if s == nil {
		s = &slot{}
		c.slots[key] = s
	}
	if s.n >= c.max {
		log := sample && sampleLog(s.lastLogged, now)
		if log {
			// Stamped only when a line is written; see sampleLog's contract.
			s.lastLogged = now
		}
		return func() {}, false, log
	}
	s.n++
	var once sync.Once
	return func() { once.Do(func() { c.release(key) }) }, true, false
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
