package ratelimit

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
)

// lockedBuffer is a concurrency-safe log sink. slog.Logger itself is
// concurrency-safe, but the underlying io.Writer it wraps is not guaranteed
// to be — a plain strings.Builder is not safe against concurrent Write calls,
// so tests that could ever drive the handler from more than one goroutine
// need this rather than a bare builder.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// requestWithPrincipal builds a GET request carrying p in context, exactly
// as auth.RequireAuth would leave it for HTTP to read via
// auth.PrincipalFrom.
func requestWithPrincipal(p auth.Principal) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	return r.WithContext(auth.WithPrincipal(r.Context(), p))
}

// rejectedCounterValue sums every recorded orbeat.ratelimit.rejected data
// point. Used instead of a single Collect-and-compare because a Sum's data
// points are not guaranteed to be a single point once more than one
// attribute-set combination has been recorded.
func rejectedCounterValue(t *testing.T, rdr sdkmetric.Reader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "orbeat.ratelimit.rejected" {
				continue
			}
			sum, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("orbeat.ratelimit.rejected has unexpected data type %T", md.Data)
			}
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

func newTestObservability() (Observability, sdkmetric.Reader, *lockedBuffer) {
	rdr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr))
	m := telemetry.NewMetrics(mp.Meter("orbeat-ratelimit-test"))
	var logBuf lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	return Observability{Metrics: m, Logger: logger}, rdr, &logBuf
}

// TestHTTPCounterIncrementsOnlyOnRejection is Task 7's mandatory "metrics
// test": it must fail if the counter is never incremented on a rejection,
// and it must ALSO fail if the counter is incremented on an allowed request
// too — a counter that counts everything is as useless as one that counts
// nothing, so both directions are asserted against the SAME counter value.
func TestHTTPCounterIncrementsOnlyOnRejection(t *testing.T) {
	obs, rdr, _ := newTestObservability()

	l := New(10, 1, time.Minute, 100) // burst 1: the second call in the same instant is rejected
	defer l.Close()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := HTTP(l, denyRateLimitedForTest, obs, next)

	p := auth.Principal{Subject: "u1", ClientID: "c1"}

	// First request: within burst, ALLOWED. The counter must stay at 0.
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, requestWithPrincipal(p))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200 (within burst)", rec1.Code)
	}
	if got := rejectedCounterValue(t, rdr); got != 0 {
		t.Fatalf("counter after one ALLOWED request = %d, want 0 (counter must not fire on allow)", got)
	}

	// Second request: burst exhausted, REJECTED. The counter must now read 1.
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, requestWithPrincipal(p))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", rec2.Code)
	}
	if got := rejectedCounterValue(t, rdr); got != 1 {
		t.Fatalf("counter after one REJECTED request = %d, want 1", got)
	}
}

// TestHTTPRejectedLogContainsExportedMessage is Task 7's mandatory
// positive-control test (plan correction C5): it proves the log line a
// rejection actually emits contains RejectedLogMessage's exact current
// value, so scripts/smoke.sh's NEGATIVE grep for that same literal
// (Task 8) has a proof the pattern CAN match before anyone trusts that it
// does not.
func TestHTTPRejectedLogContainsExportedMessage(t *testing.T) {
	obs, _, logBuf := newTestObservability()

	l := New(10, 1, time.Minute, 100)
	defer l.Close()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := HTTP(l, denyRateLimitedForTest, obs, next)

	p := auth.Principal{Subject: "u2", ClientID: "c1"}

	handler.ServeHTTP(httptest.NewRecorder(), requestWithPrincipal(p)) // allowed, drains the sole token
	handler.ServeHTTP(httptest.NewRecorder(), requestWithPrincipal(p)) // rejected

	if got := logBuf.String(); !strings.Contains(got, RejectedLogMessage) {
		t.Fatalf("log output does not contain RejectedLogMessage %q; got: %s", RejectedLogMessage, got)
	}
}

// TestHTTPLogIsSampledNotPerRejection pins that the HTTP adapter forwards the
// limiter's sampling decision instead of asking for a line on every rejection:
// a client hammering a drained bucket must still increment the counter every
// time (the durable instrument) while producing only ONE log line (the
// breadcrumb).
//
// WHAT IT DOES NOT PROVE, stated because it used to be read as proving it.
// This was the whole gate on spec §9's "at most once per key per window", and
// it passed on a sampler that was defeated by any sustained overload: 25
// rejections in a tight loop leave no room for a token to refill, so no
// allowed request lands between them, and the per-streak flag that shipped was
// only ever cleared by an allowed request. One line here was equally true of
// the broken code and the fixed code. The traffic shape that actually occurs,
// a client above its limit getting one request in every 1/rps admitted, is
// covered in sample_test.go against a synthetic clock, where the same scenario
// produced 241 lines for 241 rejections before the fix.
func TestHTTPLogIsSampledNotPerRejection(t *testing.T) {
	obs, rdr, logBuf := newTestObservability()

	l := New(10, 1, time.Minute, 100)
	defer l.Close()

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := HTTP(l, denyRateLimitedForTest, obs, next)

	p := auth.Principal{Subject: "u3", ClientID: "c1"}

	handler.ServeHTTP(httptest.NewRecorder(), requestWithPrincipal(p)) // allowed, drains the sole token
	const rejections = 25
	for i := 0; i < rejections; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, requestWithPrincipal(p))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("rejection %d = %d, want 429", i, rec.Code)
		}
	}

	if got := rejectedCounterValue(t, rdr); got != rejections {
		t.Fatalf("counter = %d, want %d (every rejection must still count)", got, rejections)
	}
	if got := strings.Count(logBuf.String(), RejectedLogMessage); got != 1 {
		t.Fatalf("log contains RejectedLogMessage %d times across %d consecutive rejections, want exactly 1 (sampled, not per-rejection)", got, rejections)
	}
}

func denyRateLimitedForTest(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusTooManyRequests)
}
