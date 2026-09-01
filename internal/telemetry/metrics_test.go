package telemetry

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricsCounterRecords(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	m := NewMetrics(mp.Meter("orbeat-test"))
	m.ArtifactApprove.Add(context.Background(), 1)

	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == "orbeat.artifact.approve" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("orbeat.artifact.approve counter not recorded")
	}
}

// TestMetricsRateLimitRejectedCounterRecords pins that NewMetrics wires up
// orbeat.ratelimit.rejected the same way as every sibling counter (Task 7,
// spec §9). The mutant-catching assertions that the counter increments ONLY
// on a real rejection — never on an allowed request — live in
// internal/ratelimit (TestHTTPCounterIncrementsOnlyOnRejection), because
// only the adapters can drive a genuine allow/reject decision; this test
// just proves the instrument itself exists and is wired into NewMetrics.
func TestMetricsRateLimitRejectedCounterRecords(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	m := NewMetrics(mp.Meter("orbeat-test"))
	m.RateLimitRejected.Add(context.Background(), 1)

	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == "orbeat.ratelimit.rejected" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("orbeat.ratelimit.rejected counter not recorded")
	}
}

// TestMetricsSessionsReclaimedCounterRecords pins that NewMetrics wires up
// orbeat.gateway.sessions.reclaimed the same way as every sibling counter
// (gateway session lifecycle design, 2026-08-16 §5). The per-cause
// assertions (that each distinct reclamation path increments it) live in
// internal/gateway/session_metrics_test.go, which alone can drive a real
// eviction; this test just proves the instrument itself exists.
func TestMetricsSessionsReclaimedCounterRecords(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	m := NewMetrics(mp.Meter("orbeat-test"))
	m.SessionsReclaimed.Add(context.Background(), 1)

	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name == "orbeat.gateway.sessions.reclaimed" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("orbeat.gateway.sessions.reclaimed counter not recorded")
	}
}

// TestUpstreamConnectDurationIsHistogram pins that
// orbeat.gateway.upstream.connect.duration is recorded, and recorded as a
// HISTOGRAM — not a counter or gauge, which would silently discard the
// distribution the instrument exists to capture.
func TestUpstreamConnectDurationIsHistogram(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	m := NewMetrics(mp.Meter("orbeat-test"))
	m.UpstreamConnect.Record(context.Background(), 0.2)

	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "orbeat.gateway.upstream.connect.duration" {
				continue
			}
			found = true
			if _, ok := md.Data.(metricdata.Histogram[float64]); !ok {
				t.Fatalf("orbeat.gateway.upstream.connect.duration data type = %T, want metricdata.Histogram[float64]", md.Data)
			}
		}
	}
	if !found {
		t.Fatalf("orbeat.gateway.upstream.connect.duration not recorded")
	}
}

// TestUpstreamConnectDurationBucketBoundaries is the test that matters: it
// pins the EXACT explicit bucket boundaries, in seconds, tuned for a dial
// bounded above by upstreamDialTimeout (10s). The SDK's default boundaries
// (sdk/metric@v1.45.0/reader.go:206) are tuned for milliseconds — {0, 5, 10,
// 25, ...} — so with a seconds unit every dial would land in the first
// bucket [0,5] and the histogram would distinguish nothing. Asserting only
// that a value was recorded passes on the defaults; this test asserts the
// boundaries themselves, which is the only assertion that fails on that bug.
func TestUpstreamConnectDurationBucketBoundaries(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	m := NewMetrics(mp.Meter("orbeat-test"))
	m.UpstreamConnect.Record(context.Background(), 0.2)

	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	var got []float64
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "orbeat.gateway.upstream.connect.duration" {
				continue
			}
			hist, ok := md.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("orbeat.gateway.upstream.connect.duration data type = %T, want metricdata.Histogram[float64]", md.Data)
			}
			for _, dp := range hist.DataPoints {
				found = true
				got = dp.Bounds
			}
		}
	}
	if !found {
		t.Fatalf("orbeat.gateway.upstream.connect.duration has no data points")
	}
	if len(got) != len(want) {
		t.Fatalf("orbeat.gateway.upstream.connect.duration bounds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orbeat.gateway.upstream.connect.duration bounds = %v, want %v", got, want)
		}
	}
}

// TestSessionLookupCounterRecords pins that orbeat.gateway.sessions.lookup is
// recorded as a counter (the ratio is computed at query time, never stored).
func TestSessionLookupCounterRecords(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	m := NewMetrics(mp.Meter("orbeat-test"))
	m.SessionLookup.Add(context.Background(), 1)

	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "orbeat.gateway.sessions.lookup" {
				continue
			}
			if _, ok := md.Data.(metricdata.Sum[int64]); !ok {
				t.Fatalf("orbeat.gateway.sessions.lookup data type = %T, want metricdata.Sum[int64]", md.Data)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("orbeat.gateway.sessions.lookup counter not recorded")
	}
}

// TestMetricsPinnedPayloadRaceFallbackCounterRecords pins that NewMetrics
// wires up orbeat.sync.pin.payload_race_fallback the same way as every
// sibling counter above. The behavior-specific assertions (that
// resolveSyncPayload increments it ONLY on the missing-payload arm, never on
// an ordinary served pin) live in internal/api, the one package that can
// drive the real decision function; this test just proves the instrument
// itself exists and is wired into NewMetrics, which is the registration half
// open-points.md's row asks for regardless of whether the fallback branch
// itself can be driven through a running handler.
func TestMetricsPinnedPayloadRaceFallbackCounterRecords(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	m := NewMetrics(mp.Meter("orbeat-test"))
	m.PinnedPayloadRaceFallback.Add(context.Background(), 1)

	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "orbeat.sync.pin.payload_race_fallback" {
				continue
			}
			if _, ok := md.Data.(metricdata.Sum[int64]); !ok {
				t.Fatalf("orbeat.sync.pin.payload_race_fallback data type = %T, want metricdata.Sum[int64]", md.Data)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("orbeat.sync.pin.payload_race_fallback counter not recorded")
	}
}

// TestRegisterSessionGaugeReadsCallback pins RegisterSessionGauge's wiring:
// the observable gauge reports whatever count() returns at collection time.
func TestRegisterSessionGaugeReadsCallback(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	n := int64(3)
	if err := RegisterSessionGauge(mp.Meter("orbeat-test"), func() int64 { return n }); err != nil {
		t.Fatalf("RegisterSessionGauge: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var got int64 = -1
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "orbeat.gateway.sessions.live" {
				continue
			}
			gauge, ok := md.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("unexpected data type %T for %s", md.Data, md.Name)
			}
			for _, dp := range gauge.DataPoints {
				got = dp.Value
			}
		}
	}
	if got != n {
		t.Fatalf("orbeat.gateway.sessions.live = %d, want %d", got, n)
	}
}
