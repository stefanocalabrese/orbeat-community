package ratelimit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSampledLogSurvivesInterleavedAllowsAtShippedGatewayRate is the A18 gate,
// and it is a replay of the measurement that found the defect: the gateway's
// shipped rps=20 burst=60 (cmd/gateway/main.go) against a client sending 25
// rps for 60 seconds.
//
// The traffic shape IS the test. The gate this replaces,
// TestHTTPLogIsSampledNotPerRejection, fires 25 rejections in a tight loop with
// no allowed request between them, so no token can refill, and it asserts
// exactly one log line — which the broken per-streak flag also produced. A
// client merely above its limit is not throttled to silence: rate.Limiter
// refills continuously, so one request every 1/rps seconds is admitted, and
// under the old code each admission cleared the flag and re-armed the log.
// Measured on this exact scenario before the fix: 241 rejections, 241 lines.
//
// The clock is synthetic, threaded through AllowAtSampled. Nothing sleeps and
// nothing reads time.Now, so the result cannot move with machine load.
func TestSampledLogSurvivesInterleavedAllowsAtShippedGatewayRate(t *testing.T) {
	l := New(20, 60, 10*time.Minute, 10000) // the gateway's shipped tools/call budget
	defer l.Close()

	const (
		clientRPS = 25
		seconds   = 60
	)
	start := time.Unix(0, 0)
	var allowed, rejected, logged int
	for i := 0; i < clientRPS*seconds; i++ {
		now := start.Add(time.Duration(i) * time.Second / clientRPS)
		ok, _, logRejection := l.AllowAtSampled("alice|cli", now)
		switch {
		case ok:
			allowed++
		default:
			rejected++
			if logRejection {
				logged++
			}
		}
	}

	// Guards against the whole thing passing vacuously. A run that produced no
	// rejections, or one that never let a request through, would satisfy a bare
	// line-count assertion while exercising nothing.
	if rejected < 100 {
		t.Fatalf("rejected = %d, want a sustained overload (>=100) — the scenario did not reproduce", rejected)
	}
	if allowed < clientRPS {
		t.Fatalf("allowed = %d, want the refill to keep admitting requests — without interleaved allows this test cannot fail for the reason it exists", allowed)
	}

	// One line per key per interval, plus the one that opens the incident.
	maxLines := 1 + int(time.Duration(seconds)*time.Second/logSampleInterval)
	if logged > maxLines {
		t.Fatalf("logged %d lines for %d rejections over %ds; want at most %d (one per %v, plus the opener)",
			logged, rejected, seconds, maxLines, logSampleInterval)
	}
	// A sampler that never logs is worse than one that logs too much: the
	// counter says a rejection happened, and nothing says which key.
	if logged < 1 {
		t.Fatalf("logged %d lines across %d rejections; the incident must produce at least one breadcrumb", logged, rejected)
	}
}

// TestOneAllowedRequestDoesNotReArmTheLog is the same defect stripped to three
// calls, so a failure names the mechanism rather than a ratio.
//
// Reject, wait just long enough for exactly one token, spend it, reject again.
// Under the flag this logged twice; under a wall-clock window, once.
func TestOneAllowedRequestDoesNotReArmTheLog(t *testing.T) {
	l := New(20, 1, 10*time.Minute, 100)
	defer l.Close()

	t0 := time.Unix(0, 0)
	if ok, _, _ := l.AllowAtSampled("k", t0); !ok {
		t.Fatal("setup: the burst token must be admitted")
	}
	if _, _, log := l.AllowAtSampled("k", t0); !log {
		t.Fatal("the first rejection must log")
	}
	// 1/20s later a token has refilled. This is the request the old flag reset on.
	tRefill := t0.Add(time.Second / 20)
	if ok, _, _ := l.AllowAtSampled("k", tRefill); !ok {
		t.Fatal("setup: a token must have refilled — without an ADMITTED call in the middle this proves nothing")
	}
	if _, _, log := l.AllowAtSampled("k", tRefill); log {
		t.Fatal("a second line inside the sample interval: an admitted request re-armed the log, which is the defect")
	}
}

// TestSampledLogFiresAgainAfterTheInterval is the other direction, and it has
// to exist: a sampler that stamps lastLogged on every rejection rather than
// only on the ones it logs would pass both tests above and then go silent for
// the entire incident. Only the opening line would ever be written.
func TestSampledLogFiresAgainAfterTheInterval(t *testing.T) {
	// rps effectively zero (the idiom internal/gateway's ratelimit_test.go
	// already uses), so nothing refills inside the interval and every call
	// below is genuinely a rejection. Written first at rps=1, where a token
	// refills every second and the loop was silently ADMITTING instead of
	// rejecting, which made the final assertion fail for a reason the comment
	// did not predict.
	l := New(0.001, 1, 10*time.Minute, 100)
	defer l.Close()

	t0 := time.Unix(0, 0)
	l.AllowAtSampled("k", t0) // drains the token
	if _, _, log := l.AllowAtSampled("k", t0); !log {
		t.Fatal("the first rejection must log")
	}
	// Rejections continue throughout, each one restamping lastLogged under the
	// mutant this test exists for.
	for d := time.Second; d < logSampleInterval; d += time.Second {
		ok, _, log := l.AllowAtSampled("k", t0.Add(d))
		if ok {
			t.Fatalf("call at +%v was ADMITTED; this test needs unbroken rejections or its final assertion proves nothing", d)
		}
		if log {
			t.Fatalf("logged again after %v, inside the %v interval", d, logSampleInterval)
		}
	}
	if _, _, log := l.AllowAtSampled("k", t0.Add(logSampleInterval)); !log {
		t.Fatalf("no line after %v of unbroken rejections: the sampler went permanently silent", logSampleInterval)
	}
}

// TestSampleLogTreatsTheZeroValueAsNeverLogged pins the branch a key's very
// first rejection takes.
func TestSampleLogTreatsTheZeroValueAsNeverLogged(t *testing.T) {
	if !sampleLog(time.Time{}, time.Unix(0, 0)) {
		t.Fatal("a key that has never logged must log its first rejection")
	}
}

// TestConcurrencyRejectionLogIsSampled covers the concurrency cap's half.
// AcquireAtSampled must behave exactly like the token bucket's sampler: one
// line, then silence for the interval, then another.
func TestConcurrencyRejectionLogIsSampled(t *testing.T) {
	c := NewConcurrency(1, 10*time.Minute)
	defer c.Close()

	t0 := time.Unix(0, 0)
	release, ok, _ := c.AcquireAtSampled("alice", t0)
	if !ok {
		t.Fatal("setup: the first acquire must succeed")
	}
	defer release()

	if _, ok, log := c.AcquireAtSampled("alice", t0); ok || !log {
		t.Fatalf("first rejection: admitted=%v logged=%v, want admitted=false logged=true", ok, log)
	}
	for d := time.Second; d < logSampleInterval; d += 5 * time.Second {
		if _, _, log := c.AcquireAtSampled("alice", t0.Add(d)); log {
			t.Fatalf("logged again after %v, inside the %v interval", d, logSampleInterval)
		}
	}
	if _, _, log := c.AcquireAtSampled("alice", t0.Add(logSampleInterval)); !log {
		t.Fatalf("no line after %v: the concurrency sampler went permanently silent", logSampleInterval)
	}
}

// TestNonReportingCallersDoNotSpendTheSamplerBudget pins the property that
// separates Allow/Acquire from their Sampled siblings: a caller that will not
// write a line must not consume the one a reporting caller is about to write.
//
// Both used to delegate to the sampled path and discard its third return,
// which is not the same as opting out. sampleLog stamps lastLogged when it
// DECIDES to log, so the discarded decision was still spent: one Acquire on a
// capped key silenced the next real AcquireSampled for a whole
// logSampleInterval, and the line that went missing is the FIRST rejection for
// that key, the only one an operator was ever going to see.
//
// Test-only today, since every production call site is a Sampled one. It is
// gated anyway because the next non-reporting caller added is exactly the kind
// of change nothing else in this package can fail for.
func TestNonReportingCallersDoNotSpendTheSamplerBudget(t *testing.T) {
	t0 := time.Unix(0, 0)

	t.Run("concurrency", func(t *testing.T) {
		c := NewConcurrency(1, 10*time.Minute)
		defer c.Close()
		release, ok := c.Acquire("alice")
		if !ok {
			t.Fatal("setup: the first acquire must succeed")
		}
		defer release()

		// A rejection through the non-reporting entry point. Under the old
		// code this stamped lastLogged.
		if _, ok := c.Acquire("alice"); ok {
			t.Fatal("setup: the second acquire must be rejected, or this asserts nothing")
		}
		if _, _, log := c.AcquireAtSampled("alice", t0); !log {
			t.Error("the first REPORTED rejection did not log: an Acquire that logs nothing spent the sampler's budget")
		}
	})

	t.Run("token bucket", func(t *testing.T) {
		// rps effectively zero, the idiom the tests above use, so nothing
		// refills and every call after the first is genuinely a rejection.
		l := New(0.001, 1, 10*time.Minute, 100)
		defer l.Close()

		if ok, _ := l.AllowAt("k", t0); !ok {
			t.Fatal("setup: the first call must be admitted")
		}
		if ok, _ := l.AllowAt("k", t0); ok {
			t.Fatal("setup: the second call must be rejected, or this asserts nothing")
		}
		if _, _, log := l.AllowAtSampled("k", t0); !log {
			t.Error("the first REPORTED rejection did not log: an AllowAt that logs nothing spent the sampler's budget")
		}
	})
}

// TestMCPConcurrencyLogIsSampledNotPerRejection is the wiring half, and it is
// the one that would have caught the shipped defect: MCPConcurrency passed a
// literal true to reportRejected, so every capped call warned while the doc
// comment on reportRejected said the line was sampled. Nothing asserted it,
// because every other MCPConcurrency test passes Observability{} and therefore
// has no logger at all.
//
// This drives the real middleware rather than the limiter, so it fails if the
// sampling decision is computed correctly and then thrown away at the call
// site, which is exactly what happened.
func TestMCPConcurrencyLogIsSampledNotPerRejection(t *testing.T) {
	obs, rdr, logBuf := newTestObservability()

	c := NewConcurrency(1, 10*time.Minute)
	defer c.Close()
	mw := MCPConcurrency(c, "tools/call", fixedKeyFn("alice", true), obs)

	release, ok := c.Acquire("alice")
	if !ok {
		t.Fatal("setup: the first acquire must succeed")
	}
	defer release()

	const rejections = 25
	for i := 0; i < rejections; i++ {
		if _, err := mw(nopHandler)(context.Background(), "tools/call", mcp.Request(nil)); err == nil {
			t.Fatalf("call %d above the cap should have been rejected", i)
		}
	}

	// Every rejection still counts: the counter is the durable instrument and
	// is deliberately not sampled.
	if got := rejectedCounterValue(t, rdr); got != rejections {
		t.Fatalf("counter = %d, want %d (every rejection must still count)", got, rejections)
	}
	if got := strings.Count(logBuf.String(), RejectedLogMessage); got != 1 {
		t.Fatalf("log contains RejectedLogMessage %d times across %d rejections, want exactly 1 — a literal true at the reportRejected call site opts out of the sampler entirely", got, rejections)
	}
}
