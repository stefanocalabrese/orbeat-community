// Package ratelimit provides per-key token-bucket limiting shared by
// orbeat-api and orbeat-gateway.
//
// now is a PARAMETER on the primitive rather than read from the clock, because
// x/time/rate exposes no injectable clock (only AllowN(t, n), TokensAt(t), …).
// Threading it is the only way refill behaviour is deterministically testable
// without time.Sleep.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type bucket struct {
	lim      *rate.Limiter
	lastUsed time.Time
	// lastLogged is when this key last produced a sampled rejection log line,
	// zero if it never has (spec §9: log at most once per key per window, not
	// per rejection). The window is wall-clock, logSampleInterval wide; see
	// sample.go for why, and for the measurement that retired the flag this
	// field replaced. Piggybacks on the bucket the Limiter already keeps
	// (bounded by ttl + maxEntries) instead of a second, separately-unbounded
	// map.
	lastLogged time.Time
}

// Limiter maps keys to token buckets, bounded by ttl and maxEntries.
type Limiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	rps        float64
	burst      int
	ttl        time.Duration
	maxEntries int
	stop       chan struct{}
	stopOnce   sync.Once
}

// New returns a Limiter. rps <= 0 disables limiting entirely (Allow always
// returns true) — the sentinel style this repo already uses for
// ORBEAT_AUDIT_RETENTION_DAYS and ORBEAT_AUDIT_EXPORT_MAX_ROWS.
//
// burst is clamped to a minimum of 1: x/time/rate's reserveN rejects whenever
// n > burst, so a literal burst of 0 would deny EVERY request — a typo turning
// an availability feature into a total outage.
func New(rps float64, burst int, ttl time.Duration, maxEntries int) *Limiter {
	if burst < 1 {
		burst = 1
	}
	l := &Limiter{
		buckets:    make(map[string]*bucket),
		rps:        rps,
		burst:      burst,
		ttl:        ttl,
		maxEntries: maxEntries,
		stop:       make(chan struct{}),
	}
	go l.run()
	return l
}

func (l *Limiter) run() {
	t := time.NewTicker(l.ttl)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-t.C:
			l.sweep(now)
		}
	}
}

// Close stops the background sweeper.
func (l *Limiter) Close() error {
	l.stopOnce.Do(func() { close(l.stop) })
	return nil
}

// Len reports the number of live buckets. It exists for tests: eviction is
// otherwise unobservable, and an unobservable property cannot be pinned.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Allow is AllowAt(key, time.Now()).
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	return l.AllowAt(key, time.Now())
}

// AllowAt reports whether the key may proceed at now, and if not, how long
// until it could.
//
// retryAfter is computed from TokensAt, never from Reserve: ReserveN CONSUMES
// tokens and reserves into the future, and CancelAt restores them only "as much
// as possible". Deriving the hint from Reserve would charge a token for the
// rejection itself, so a client hammering above the limit would push its own
// recovery outward on every rejected request and never recover.
//
// Like ConcurrencyLimiter.Acquire, it does not participate in sampling: it
// used to call decideAt and drop the third return, which stamped lastLogged on
// behalf of a caller that was never going to log, silencing the next real
// AllowAtSampled for a whole logSampleInterval. Same defect, same file pair,
// found by grepping the class after fixing the concurrency half.
func (l *Limiter) AllowAt(key string, now time.Time) (bool, time.Duration) {
	allowed, retryAfter, _ := l.decideAt(key, now, false)
	return allowed, retryAfter
}

// AllowAtSampled is AllowAt plus a third return, logRejection: true only for
// the first rejection for key within logSampleInterval of the last one that
// logged (Task 7, spec §9: "log at most once per key per window, not per
// rejection"). The window is wall-clock, measured on the SAME now the bucket
// decision uses, which is what makes it deterministically testable with no
// sleeping.
//
// It used to be a per-streak flag any allowed request cleared, on the claim
// that a streak IS the window. That claim was false and the sampler was
// defeated by ordinary sustained overload; sample.go carries the measurement
// and the replacement's reasoning.
//
// It delegates to the SAME locked decision as AllowAt (via decideAt) rather
// than re-deriving allowed/retryAfter independently, so the two can never
// disagree and a rejection is never double-charged against the bucket.
func (l *Limiter) AllowAtSampled(key string, now time.Time) (allowed bool, retryAfter time.Duration, logRejection bool) {
	return l.decideAt(key, now, true)
}

// AllowSampled is AllowAtSampled(key, time.Now()).
func (l *Limiter) AllowSampled(key string) (bool, time.Duration, bool) {
	return l.AllowAtSampled(key, time.Now())
}

// decideAt is the single locked decision point AllowAt and AllowAtSampled
// both delegate to, so their allow/reject/retryAfter outcomes can never drift
// apart from each other.
//
// sample tells it whether the caller will actually write the line. False means
// the sampler is neither read nor stamped, so a non-reporting caller cannot
// spend a reporting one's budget; see AllowAt.
func (l *Limiter) decideAt(key string, now time.Time, sample bool) (bool, time.Duration, bool) {
	if l.rps <= 0 {
		return true, 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		l.evictIfFullLocked(now)
		b = &bucket{lim: rate.NewLimiter(rate.Limit(l.rps), l.burst)}
		l.buckets[key] = b
	}
	// Stamped on EVERY call, rejections included (spec §3.1 invariant 1).
	b.lastUsed = now

	if b.lim.AllowN(now, 1) {
		// Deliberately does NOT touch lastLogged. An admitted request is what
		// the old flag reset, and resetting on it is exactly what let a client
		// steadily over the limit log every rejection: rate.Limiter refills
		// continuously, so one request in every 1/rps seconds gets through.
		return true, 0, false
	}
	logRejection := sample && sampleLog(b.lastLogged, now)
	if logRejection {
		// Stamped only when a line is actually written. Stamping on every
		// rejection would keep pushing the window out under sustained
		// overload and silence every line after the first.
		b.lastLogged = now
	}
	tokens := b.lim.TokensAt(now)
	need := 1 - tokens
	if need < 0 {
		need = 0
	}
	return false, time.Duration(need / l.rps * float64(time.Second)), logRejection
}

// sweep drops buckets idle since before now-ttl and returns how many went.
func (l *Limiter) sweep(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.ttl)
	n := 0
	for k, b := range l.buckets {
		if b.lastUsed.Before(cutoff) {
			delete(l.buckets, k)
			n++
		}
	}
	return n
}

// evictIfFullLocked makes room for one new key by dropping the
// least-recently-used bucket. Never rejects (that would 429 a brand-new caller
// over an internal memory bound) and never bypasses (that would let anyone
// disable the limiter by minting keys). Caller holds l.mu.
func (l *Limiter) evictIfFullLocked(now time.Time) {
	if l.maxEntries <= 0 || len(l.buckets) < l.maxEntries {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, b := range l.buckets {
		if oldestKey == "" || b.lastUsed.Before(oldest) {
			oldestKey, oldest = k, b.lastUsed
		}
	}
	delete(l.buckets, oldestKey)
}
