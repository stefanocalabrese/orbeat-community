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

	reclaimEntitlementChange = "entitlement_change" // invalidateTenant(): the entitlement-change nudge.
)

// Results for the orbeat.gateway.sessions.lookup counter (gateway
// upstream-connect and session-cache metrics design, 2026-08-18 §2/§4).
const (
	lookupHit  = "hit"
	lookupMiss = "miss"
)

// session is one caller's (subject's) assembled gateway state.
type session struct {
	subject string
	// cacheKey is the string sessionCache.m actually stores this session
	// under -- set by sessionCache.put, from the key its caller passed to
	// getOrBuild, NOT derived from subject (fable-audit B14: production
	// callers key on ratelimit.KeyFor(p), "subject|azp", so two sessions for
	// the SAME subject via two different OAuth clients are DISTINCT map
	// entries sharing one subject value). sweepTransportsLocked is the one
	// reader: given only a *session (from a transportBinding), it has to ask
	// "is this still c.m's current entry", and c.m is keyed by cacheKey, not
	// by subject.
	cacheKey     string
	tenantID     string
	actor        string
	entitlements []store.Entitlement
	slugToServer map[string]string
	upstreams    []*upstreamConn
	mcpServer    *mcp.Server
	lastSeen     time.Time
	builtAt      time.Time
	// keyID is non-empty when this session belongs to a virtual key rather
	// than a human (internal/gateway/virtualkey.ee.go, docs/specs/2026-08-25-
	// orbeat-virtual-keys-design.md §7). keyNarrow is that key's RAW,
	// NAMESPACED allowed_tools row (slug__tool strings; nil means the role's
	// full grant) -- it is NOT filtered to any one server yet. Task 4's
	// toolCallAllowed does that filtering per call, because only it knows
	// which server is being checked; rbac.KeyToolAllowed must never see this
	// field unfiltered (spec §6, "the second trap"). Both fields are read by
	// rbacMiddleware on every call.
	keyID     string
	keyNarrow []string
	// keyClientID is the key's Keycloak client_id (buildSession copies it
	// straight from the resolved auth.Principal whenever keyID is non-empty,
	// server.go) -- the exact identifier store.GetVirtualKeyByClientID takes,
	// and the one migration 00020's virtual_key_lookup index is built to
	// serve. keyID (the store row's uuid) cannot be used for this: the store
	// exposes no by-id reader, only GetVirtualKeyByClientID and
	// RevokeVirtualKey(id). rbacMiddleware's per-call revocation check (spec
	// §8) reads this field, never keyID, to ask the store "is this key
	// revoked right now" -- deliberately NOT cached on the session itself,
	// which is exactly the cache a leaked-and-revoked credential must not be
	// able to hide behind.
	keyClientID string
	// transportIDs is every Mcp-Session-Id the SDK minted while this session
	// was the subject's current one -- the reverse of sessionCache.bound, kept
	// so eviction can tombstone its OWN bindings without scanning that whole
	// map. Read and written ONLY under sessionCache.mu (bindTransport and
	// staleTransportsLocked are its only two writers), which is why it is a
	// plain slice and not an atomic or a mutex-carrying field: unlike dirty, no
	// proxy closure ever touches it.
	transportIDs []string
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

// transportBinding links one Mcp-Session-Id to the gateway session it was born
// from (A1). sess is the identity being compared, and a rebuild always produces
// a FRESH *session, so for the subject who minted the id "the binding's sess is
// not my current session" means exactly "this transport session was built from
// gateway state that no longer exists". It does NOT mean that in general: a
// replay of an id minted for a DIFFERENT subject also fails the comparison
// while b.sess is that subject's perfectly live session, which is why
// withSession splits the two cases apart (cross_subject) rather than reporting
// one cause for both.
//
// Once that session is reclaimed the entry becomes a TOMBSTONE: sess is
// nil-ed (releasing the dead session's entitlements, slug map and upstream
// slice -- the point of pruning at every eviction path) while cause and
// staleAt survive, because a 404 that cannot say WHY is an operator's dead
// end.
//
// A tombstone is not deleted when it serves its 404, and the reason CHANGED on
// 2026-08-30. It used to be a safety property: withSession adopted an unknown
// id, so forgetting a tombstone handed the id back to the frozen *mcp.Server.
// withSession now refuses an unknown id, so a forgotten tombstone and a live
// tombstone produce the same 404 and the same client behaviour. What survives
// is diagnostic: the tombstone is the only thing that can put a REASON in the
// log line and in X-Orbeat-Session-Rebuilt, and a client that ignores the
// first 404 and replays gets the same reason again rather than a bare
// "unbound". reap drops them once tombstoneHorizon has passed.
type transportBinding struct {
	sess    *session
	cause   string
	staleAt time.Time
}

// transportLookup is what lookupTransport reports about one Mcp-Session-Id.
// It is a VALUE copy taken under the cache mutex: handing back the
// *transportBinding itself would let withSession read cause/sess while an
// eviction on another goroutine tombstones them.
type transportLookup struct {
	known bool
	sess  *session
	cause string
}

// reclaimUnknown is the cause used wherever a binding is refused that no
// eviction path ever left a reason on. TWO different situations produce it,
// and until 2026-08-30 this comment named only the first and read as though it
// were the only one:
//
//  1. The race described below: a binding whose session belongs to THIS
//     subject but is no longer the subject's current one, never tombstoned
//     because it was created after the eviction that would have tombstoned it.
//     sweepTransportsLocked stamps the same cause on one of these when a later
//     tick finds it.
//
//  2. A cross-subject replay, where the binding's session is ALIVE and belongs
//     to somebody else. Nothing was reclaimed there and nothing is unknown to
//     the gateway. "unknown" is only what the CLIENT is told, on purpose, so
//     the wire answer cannot separate a live foreign id from an id this
//     process never minted; the log line carries sessionCrossSubject
//     (server.go) instead, so an operator is never shown this value for that
//     case.
//
// THE RACE (situation 1), which is why the constant is not defensive padding
// even now that the id is bound at mint time: withSession's
// getOrBuild returns sess and releases the cache mutex, and the SDK calls
// GetSessionID a moment later, inside next.ServeHTTP. An eviction landing in
// that window (the reaper, or an entitlement nudge) tombstones the bindings
// that existed AT THAT MOMENT and clears s.transportIDs, and bindTransport
// then binds a brand-new id to an already-dead session. The next request is
// still answered correctly -- the session it was built on is gone, so it is
// 404-ed -- but nothing recorded why.
//
// Mint-time binding NARROWED that window from "the whole initialize handler"
// to "the few instructions between getOrBuild and getServer"; it did not close
// it, and no ordering of these two lock acquisitions can, because the session
// must be chosen before the id can be minted against it.
const reclaimUnknown = "unknown"

// sessionCache is a principal-keyed cache with idle (ttl) and max-age
// eviction. The key is whatever the caller passes to get/getOrBuild -- in
// production (withSession, server.go) that is ratelimit.KeyFor(p)
// ("subject|azp", falling back to bare subject when the token carries no
// azp), NOT the bare subject alone (fable-audit B14: buildSession forks on
// p.ClientID and Resolve reconciles roles from the token's own realm_access,
// both PER-CLIENT values, so two OAuth clients belonging to the same human
// can legitimately carry different roles via Keycloak's per-client role
// scope mappings -- keying on subject alone let whichever client connected
// first silently decide the other's entitlements too). The cache type
// itself is agnostic to what a key represents; every method below still
// reads "subject" as a parameter name only where session.subject -- the
// bare, un-azp-qualified caller identity used for audit Actor and the
// cross-subject-replay check in withSession -- is what's actually meant.
type sessionCache struct {
	mu sync.Mutex
	m  map[string]*session
	// bound is the Mcp-Session-Id -> gateway-session index (A1). It is owned by
	// sessionCache rather than by Server because the cache already owns every
	// eviction path, and a binding is only meaningful relative to an eviction.
	bound  map[string]*transportBinding
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
	c := &sessionCache{m: make(map[string]*session), bound: make(map[string]*transportBinding), ttl: ttl, maxAge: maxAge, done: make(chan struct{}), metrics: metrics}
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

// getOrBuild returns the cached session for key, or builds exactly one even
// under concurrent callers (single-flight), caching the result. Concurrent
// callers for the same key share the single build, preventing duplicate
// upstream connections and the orphaned-session leak that a plain
// get/build/put would cause. key is whatever the caller uses to identify a
// cacheable session -- production callers pass ratelimit.KeyFor(p), not the
// bare subject alone (see sessionCache's own doc comment, fable-audit B14).
func (c *sessionCache) getOrBuild(key string, now time.Time, build func() (*session, error)) (*session, error) {
	if s, ok := c.get(key, now); ok {
		c.recordLookup(lookupHit)
		return s, nil
	}
	c.recordLookup(lookupMiss)
	v, err, _ := c.group.Do(key, func() (any, error) {
		// Re-check under the flight: a prior flight for this key may have
		// already populated the cache.
		if s, ok := c.get(key, time.Now()); ok {
			return s, nil
		}
		s, err := build()
		if err != nil {
			return nil, err
		}
		c.put(key, s, time.Now())
		return s, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*session), nil
}

func (c *sessionCache) get(key string, now time.Time) (*session, bool) {
	c.mu.Lock()
	s, ok := c.m[key]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	if cause, expired := c.reclaimCause(s, now); expired {
		// Tombstone before the delete, while s is still reachable: the very
		// next request from a client still holding one of this session's
		// Mcp-Session-Ids must get a 404 NAMING THIS CAUSE (A1). Without the
		// tombstone the request would still be refused -- withSession has no
		// branch that adopts an id it holds no binding for -- but as a bare
		// "unbound", with the reason lost.
		c.staleTransportsLocked(s, cause, now)
		delete(c.m, key)
		c.mu.Unlock()
		// close() does network I/O (HTTP DELETE per upstream); off the mutex so
		// one hung upstream cannot head-of-line-block every other cache key.
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

// bindTransport records that the MCP transport session id was created while s
// was the subject's current gateway session (A1).
//
// It has exactly ONE caller: the GetSessionID closure buildSession installs on
// s.mcpServer (server.go). The SDK invokes that closure to mint the id, so the
// binding is taken before the id exists anywhere else -- before the transport
// is constructed, before the response header is written, and therefore before
// any client can replay it. withSession no longer binds anything; it only
// reads the index and refuses what does not match.
//
// An id already in the index is left alone: rebinding it from session A to
// session B would strand it in A.transportIDs, so evicting A would then
// tombstone a binding that belongs to B. A no-op after closeAll, which is what
// keeps a session handed back by the post-close put race (getOrBuild still
// returns it) from acquiring a binding nothing would ever tombstone.
func (c *sessionCache) bindTransport(id string, s *session) {
	if id == "" || s == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if _, ok := c.bound[id]; ok {
		return
	}
	c.bound[id] = &transportBinding{sess: s}
	s.transportIDs = append(s.transportIDs, id)
}

// lookupTransport reports what the index knows about id. known=false means
// this process has no record of it at all, and withSession answers that with a
// 404 (marker "unbound") for every method except DELETE. Since every id this
// process mints is bound inside GetSessionID, an unknown id is one of: minted
// by a previous process, a tombstone already swept past tombstoneHorizon, or
// never real.
func (c *sessionCache) lookupTransport(id string) transportLookup {
	if id == "" {
		return transportLookup{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.bound[id]
	if !ok {
		return transportLookup{}
	}
	return transportLookup{known: true, sess: b.sess, cause: b.cause}
}

// staleTransportsLocked tombstones every Mcp-Session-Id bound to s, recording
// why s was reclaimed. Callers hold c.mu. It is called from every path that
// already enumerates eviction victims -- get, reap, invalidateTenant, closeAll
// and put's post-close race -- so no eviction can leave a binding pointing at
// a session that is no longer the subject's current one.
func (c *sessionCache) staleTransportsLocked(s *session, cause string, now time.Time) {
	for _, id := range s.transportIDs {
		b, ok := c.bound[id]
		if !ok || b.sess != s {
			// Already tombstoned, already swept, or (defensively) rebound
			// elsewhere -- never overwrite another session's binding.
			continue
		}
		b.sess = nil
		b.cause = cause
		b.staleAt = now
	}
	s.transportIDs = nil
}

// tombstoneHorizon is how long sweepTransportsLocked keeps a tombstone after
// staleAt before forgetting the id entirely.
//
// THIS IS NO LONGER A SAFETY BOUND, and until 2026-08-30 this comment said at
// length that it was. The old argument ran: withSession's never-seen branch
// rebinds an unknown id to the subject's CURRENT session and lets it through,
// so an id forgotten while the SDK still holds its transport session is served
// by the frozen *mcp.Server -- the A1 defect restored -- and the horizon is
// what stands between the two. That branch is gone. withSession now REFUSES an
// unknown id, so forgetting a tombstone downgrades the answer from
// "404, cause=max_age" to "404, cause=unbound" and changes nothing else: the
// SDK client maps both to mcp.ErrSessionMissing, and the frozen server is
// unreachable either way. Nothing here is load-bearing for correctness, and
// the comment must not go on implying it is.
//
// WHAT IT IS NOW: a diagnostic-retention window, and the value stays 2*maxAge
// on that footing.
//
//   - The floor is the SDK's own transport session. Handler() sets the SDK's
//     idle timeout from sessionTransportTimeout, which New sets to
//     sessionMaxAge, and New builds this cache with the same sessionMaxAge --
//     both halves pinned by
//     TestServerSessionTransportTimeoutFieldMatchesSessionMaxAge. For as long
//     as a client can still be holding a live transport session, its 404
//     should be able to name WHY the gateway session behind it went away. A
//     horizon shorter than maxAge would routinely answer "unbound" to a client
//     whose session we evicted seconds ago, which is the operator-hostile
//     case.
//   - maxAge exactly is not enough for that, for the reason the old comment
//     got right: the SDK PAUSES its idle timer for the whole duration of a
//     POST. startPOST stops it and endPOST, deferred until the response is
//     complete, resets it to a full SessionTimeout (go-sdk@v1.7.0
//     mcp/streamable.go; line numbers rot, the two method names are the
//     durable half of the citation). So the SDK can still hold a transport
//     session at staleAt+delta+maxAge. Reap ticks every ttl/2 (== maxAge,
//     since sessionTTL is 2*sessionMaxAge) and the sweep runs up to one tick
//     late, so the retained window is [2*maxAge, 3*maxAge).
//   - The cost is bounded and was reviewed as such: a tombstone is three words
//     plus a map entry, held for about twenty production minutes, and the
//     binding index has a real time bound rather than a count cap.
//
// Shortening it would trade operator-visible reasons for memory nobody is
// short of; lengthening it would retain reasons for clients the SDK has long
// since forgotten. 2*maxAge is where those meet, and
// TestSessionCacheTombstoneSurvivesUntilTheHorizon pins the value rather than
// an inequality.
func (c *sessionCache) tombstoneHorizon() time.Duration { return 2 * c.maxAge }

// sweepTransportsLocked drops tombstones older than tombstoneHorizon, bounding
// the index for clients that never come back, and tombstones any binding whose
// session is no longer the subject's current one (the reclaimUnknown race --
// without this it is a live binding nothing would ever tombstone, so nothing
// would ever expire it either). Callers hold c.mu.
func (c *sessionCache) sweepTransportsLocked(now time.Time) {
	for id, b := range c.bound {
		if b.sess != nil {
			// c.m is keyed by cacheKey ("subject|azp" in production via
			// ratelimit.KeyFor), never by bare subject (fable-audit B14) --
			// looking this up by b.sess.subject would find a DIFFERENT
			// client's current session for the same human and wrongly treat
			// a still-current binding as stale.
			if cur, ok := c.m[b.sess.cacheKey]; !ok || cur != b.sess {
				b.sess, b.cause, b.staleAt = nil, reclaimUnknown, now
			}
			continue
		}
		if now.Sub(b.staleAt) > c.tombstoneHorizon() {
			delete(c.bound, id)
		}
	}
}

// recordReclaim increments the sessions-reclaimed counter for cause. get,
// put, reap and closeAll all unlock c.mu before calling it, so metric export
// cannot block cache access. invalidateTenant is the one exception and calls
// it while still holding the lock: it holds c.mu through a deferred unlock,
// and moving this single Add out from under that would mean unlocking on both
// of its return paths.
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

// put inserts s into the cache under key -- the SAME key its caller passed
// to getOrBuild (getOrBuild's own singleflight closure is put's only
// production caller), not anything derived from s.subject. key is stamped
// onto s.cacheKey so sweepTransportsLocked, given only a *session, can look
// itself back up in c.m (fable-audit B14; see session's own cacheKey field
// doc comment).
func (c *sessionCache) put(key string, s *session, now time.Time) {
	c.mu.Lock()
	if c.closed {
		// Symmetric with every other eviction path. In practice s has no
		// bindings yet (nothing can bind before getOrBuild returns it), and
		// bindTransport refuses to add one while closed; this call is what makes
		// that a belt-and-braces statement rather than a load-bearing assumption.
		c.staleTransportsLocked(s, reclaimExplicit, now)
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
	s.cacheKey = key
	c.m[key] = s
	c.mu.Unlock()
}

// reap sweeps the cache, closing and removing every session get would treat as
// expired. Called by the background reaper so idle sessions are released even
// when their subject never returns. Victims are collected under the mutex but
// closed outside it (network I/O).
// invalidateTenant drops every cached session belonging to tenantID and
// returns how many it dropped, so a caller can log a number rather than a
// hope.
//
// It exists for the entitlement-change nudge and is deliberately a HINT, never
// a control: missing it costs at most sessionMaxAge of staleness, which is
// exactly what the cache guaranteed before any nudge existed. Nothing may come
// to depend on it having run.
func (c *sessionCache) invalidateTenant(tenantID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0
	}
	n := 0
	now := time.Now()
	for k, s := range c.m {
		if s.tenantID != tenantID {
			continue
		}
		c.staleTransportsLocked(s, reclaimEntitlementChange, now)
		delete(c.m, k)
		// Closed like the reaper closes an expired session: the upstream
		// connections are this session's, and leaving them open would leak a
		// connection set per invalidation.
		go s.close()
		n++
	}
	if n > 0 {
		c.recordReclaim(reclaimEntitlementChange)
	}
	return n
}

func (c *sessionCache) reap(now time.Time) {
	c.mu.Lock()
	type victim struct {
		s     *session
		cause string
	}
	var victims []victim
	for key, s := range c.m {
		if cause, expired := c.reclaimCause(s, now); expired {
			c.staleTransportsLocked(s, cause, now)
			delete(c.m, key)
			victims = append(victims, victim{s, cause})
		}
	}
	// The one sweep that bounds the binding index; see tombstoneHorizon for
	// why the horizon is what it is. reap is the right home for it: it is the
	// only path that runs on a timer rather than on a request, so a tombstone
	// for a client that never returns still expires.
	c.sweepTransportsLocked(now)
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
//
// WHAT HAPPENS TO THE BINDING INDEX AFTER THIS, stated because it is not
// obvious and was verified rather than assumed (review, 2026-08-30): closing
// c.done stops the reaper, so sweepTransportsLocked never runs again and every
// tombstone closeAll just wrote is retained for the lifetime of the process.
// That is not a leak, because the index cannot GROW either: bindTransport
// returns early while c.closed, so no new id is ever added. c.bound is frozen
// at exactly the size it had when closeAll ran. In production closeAll is
// gateway shutdown, so "for the lifetime of the process" is measured in
// milliseconds; in tests a Server whose Close ran keeps its tombstones, which
// is what lets a test assert on them after the fact.
func (c *sessionCache) closeAll() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	victims := make([]*session, 0, len(c.m))
	now := time.Now()
	for key, s := range c.m {
		c.staleTransportsLocked(s, reclaimExplicit, now)
		victims = append(victims, s)
		delete(c.m, key)
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
