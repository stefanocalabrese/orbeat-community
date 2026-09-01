package gateway

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
)

// newManualSessionMetrics builds a *telemetry.Metrics backed by a manual
// reader (mirrors metrics_test.go's TestRBACDecisionMetricOnlyCountsPerCallDecisions),
// plus two collectors sharing that one reader: the orbeat.gateway.sessions.reclaimed
// counter's per-cause counts, and the orbeat.gateway.sessions.lookup counter's
// per-result (hit|miss) counts. Sharing one reader lets a single test observe
// both counters moving off the same getOrBuild call (design 2026-08-18 §6
// gate 4).
func newManualSessionMetrics(t *testing.T) (metrics *telemetry.Metrics, collectReclaimed func() map[string]int64, collectLookup func() map[string]int64) {
	t.Helper()
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	m := telemetry.NewMetrics(mp.Meter("session-metrics-test"))
	collectByAttr := func(metricName, attrKey string) map[string]int64 {
		var rm metricdata.ResourceMetrics
		if err := rdr.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		got := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				if md.Name != metricName {
					continue
				}
				sum, ok := md.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("unexpected data type %T for %s", md.Data, md.Name)
				}
				for _, dp := range sum.DataPoints {
					v, _ := dp.Attributes.Value(attribute.Key(attrKey))
					got[v.AsString()] += dp.Value
				}
			}
		}
		return got
	}
	collectReclaimed = func() map[string]int64 { return collectByAttr("orbeat.gateway.sessions.reclaimed", "cause") }
	collectLookup = func() map[string]int64 { return collectByAttr("orbeat.gateway.sessions.lookup", "result") }
	return m, collectReclaimed, collectLookup
}

// The four tests below pin each of the four distinct recordReclaim call
// sites in session.go independently (design 2026-08-16 §5/§6 item 3): if any
// one of them stops incrementing the counter, exactly its own test below
// fails — not the others, since each drives a different eviction path in
// isolation.

// TestSessionCacheGetRecordsDirtyReclaim pins get()'s recordReclaim call for
// the dirty cause.
func TestSessionCacheGetRecordsDirtyReclaim(t *testing.T) {
	metrics, collect, _ := newManualSessionMetrics(t)
	c := newSessionCache(time.Hour, time.Hour, metrics)
	defer c.closeAll()

	s := &session{subject: "s", slugToServer: map[string]string{}}
	c.put("s", s, time.Now())
	s.markDirty()

	if _, hit := c.get("s", time.Now()); hit {
		t.Fatal("dirty session must be evicted, not returned as a hit")
	}

	if got := collect()[reclaimDirty]; got != 1 {
		t.Fatalf("%s reclaim count = %d, want 1", reclaimDirty, got)
	}
}

// TestSessionCacheGetRecordsMaxAgeReclaim pins get()'s recordReclaim call for
// the max_age cause. ttl is set generously large so only max-age can fire.
func TestSessionCacheGetRecordsMaxAgeReclaim(t *testing.T) {
	metrics, collect, _ := newManualSessionMetrics(t)
	c := newSessionCache(time.Hour, 5*time.Minute, metrics)
	defer c.closeAll()

	base := time.Now()
	s := &session{subject: "s", slugToServer: map[string]string{}}
	c.put("s", s, base.Add(-6*time.Minute)) // builtAt/lastSeen both 6m ago; maxAge is 5m

	if _, hit := c.get("s", base); hit {
		t.Fatal("max-age-expired session must be evicted, not returned as a hit")
	}

	if got := collect()[reclaimMaxAge]; got != 1 {
		t.Fatalf("%s reclaim count = %d, want 1", reclaimMaxAge, got)
	}
}

// TestSessionCacheReapRecordsIdleReclaim pins reap()'s recordReclaim call for
// the idle_timeout cause. ttl deliberately < maxAge (as in
// TestSessionCacheReapEvictsIdle) so only the idle rule can fire.
func TestSessionCacheReapRecordsIdleReclaim(t *testing.T) {
	metrics, collect, _ := newManualSessionMetrics(t)
	c := newSessionCache(time.Minute, time.Hour, metrics)
	defer c.closeAll()

	base := time.Now()
	s := &session{subject: "idle", slugToServer: map[string]string{}}
	c.put("idle", s, base.Add(-2*time.Minute))

	c.reap(base)

	if got := collect()[reclaimIdle]; got != 1 {
		t.Fatalf("%s reclaim count = %d, want 1", reclaimIdle, got)
	}
}

// TestSessionCacheCloseAllRecordsExplicitReclaim pins closeAll()'s
// recordReclaim call for the explicit_close cause.
func TestSessionCacheCloseAllRecordsExplicitReclaim(t *testing.T) {
	metrics, collect, _ := newManualSessionMetrics(t)
	c := newSessionCache(time.Hour, time.Hour, metrics)

	s := &session{subject: "s", slugToServer: map[string]string{}}
	c.put("s", s, time.Now())

	c.closeAll()

	if got := collect()[reclaimExplicit]; got != 1 {
		t.Fatalf("%s reclaim count = %d, want 1", reclaimExplicit, got)
	}
}

// TestSessionCachePutAfterCloseRecordsExplicitReclaim pins put()'s
// post-shutdown-race recordReclaim call (a build finishing after closeAll)
// for the explicit_close cause — a distinct call site from closeAll()'s own,
// mirroring TestSessionCachePutAfterCloseAllClosesSession.
func TestSessionCachePutAfterCloseRecordsExplicitReclaim(t *testing.T) {
	metrics, collect, _ := newManualSessionMetrics(t)
	c := newSessionCache(time.Hour, time.Hour, metrics)
	c.closeAll()

	late := &session{subject: "late", slugToServer: map[string]string{}}
	c.put("late", late, time.Now())

	if got := collect()[reclaimExplicit]; got != 1 {
		t.Fatalf("%s reclaim count (post-close put) = %d, want 1", reclaimExplicit, got)
	}
}

// TestSessionCacheSizeReflectsCache pins sessionCache.size(), the accessor
// telemetry.RegisterSessionGauge reads at collection time (design §5).
func TestSessionCacheSizeReflectsCache(t *testing.T) {
	c := newSessionCache(time.Hour, time.Hour, nil)
	defer c.closeAll()

	if got := c.size(); got != 0 {
		t.Fatalf("size on an empty cache = %d, want 0", got)
	}
	c.put("a", &session{subject: "a", slugToServer: map[string]string{}}, time.Now())
	c.put("b", &session{subject: "b", slugToServer: map[string]string{}}, time.Now())
	if got := c.size(); got != 2 {
		t.Fatalf("size after two puts = %d, want 2", got)
	}
}

// The three tests below pin orbeat.gateway.sessions.lookup (design 2026-08-18
// §4/§6): recorded once per getOrBuild call, off the optimistic get only.

// TestSessionCacheGetOrBuildRecordsHit drives a cache hit through getOrBuild
// and asserts result=hit moves by exactly one, and result=miss stays at zero.
func TestSessionCacheGetOrBuildRecordsHit(t *testing.T) {
	metrics, _, collectLookup := newManualSessionMetrics(t)
	c := newSessionCache(time.Hour, time.Hour, metrics)
	defer c.closeAll()

	s := &session{subject: "s", slugToServer: map[string]string{}}
	c.put("s", s, time.Now())

	got, err := c.getOrBuild("s", time.Now(), func() (*session, error) {
		t.Fatal("build must not run on a cache hit")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("getOrBuild: %v", err)
	}
	if got != s {
		t.Fatal("getOrBuild returned a different session than the cached one")
	}

	counts := collectLookup()
	if counts["hit"] != 1 {
		t.Fatalf("lookup hit count = %d, want 1", counts["hit"])
	}
	if counts["miss"] != 0 {
		t.Fatalf("lookup miss count = %d, want 0", counts["miss"])
	}
}

// TestSessionCacheGetOrBuildRecordsMiss drives an empty-cache miss through
// getOrBuild and asserts result=miss moves by exactly one. getOrBuild calls
// get() twice on a miss (the optimistic check, then the re-check inside the
// singleflight) — instrumenting the wrong call site would report 2 here
// instead of 1 (design 2026-08-18 §4, gate 3).
func TestSessionCacheGetOrBuildRecordsMiss(t *testing.T) {
	metrics, _, collectLookup := newManualSessionMetrics(t)
	c := newSessionCache(time.Hour, time.Hour, metrics)
	defer c.closeAll()

	built := &session{subject: "s", slugToServer: map[string]string{}}
	got, err := c.getOrBuild("s", time.Now(), func() (*session, error) { return built, nil })
	if err != nil {
		t.Fatalf("getOrBuild: %v", err)
	}
	if got != built {
		t.Fatal("getOrBuild did not return the built session")
	}

	counts := collectLookup()
	if counts["miss"] != 1 {
		t.Fatalf("lookup miss count = %d, want 1 (a double-count at get() would report 2)", counts["miss"])
	}
	if counts["hit"] != 0 {
		t.Fatalf("lookup hit count = %d, want 0", counts["hit"])
	}
}

// TestSessionCacheGetOrBuildExpiryCountsOneReclaimAndOneMiss drives a
// max-age-expired session through getOrBuild and asserts SessionsReclaimed
// moves by exactly one AND the lookup counter's result=miss moves by exactly
// one — not the lookup counter by two, since the reclaim get() call finding
// nothing found is a second event, already counted by SessionsReclaimed, not
// a second miss (design 2026-08-18 §4, gate 4).
func TestSessionCacheGetOrBuildExpiryCountsOneReclaimAndOneMiss(t *testing.T) {
	metrics, collectReclaimed, collectLookup := newManualSessionMetrics(t)
	c := newSessionCache(time.Hour, 5*time.Minute, metrics)
	defer c.closeAll()

	base := time.Now()
	stale := &session{subject: "s", slugToServer: map[string]string{}}
	c.put("s", stale, base.Add(-6*time.Minute)) // builtAt/lastSeen both 6m ago; maxAge is 5m

	rebuilt := &session{subject: "s", slugToServer: map[string]string{}}
	got, err := c.getOrBuild("s", base, func() (*session, error) { return rebuilt, nil })
	if err != nil {
		t.Fatalf("getOrBuild: %v", err)
	}
	if got != rebuilt {
		t.Fatal("getOrBuild did not rebuild past the max-age ceiling")
	}

	if rc := collectReclaimed()[reclaimMaxAge]; rc != 1 {
		t.Fatalf("%s reclaim count = %d, want 1", reclaimMaxAge, rc)
	}
	if mc := collectLookup()["miss"]; mc != 1 {
		t.Fatalf("lookup miss count = %d, want 1 (an expiry must not also inflate this to 2)", mc)
	}
}
