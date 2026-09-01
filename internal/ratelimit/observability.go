package ratelimit

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
)

// RejectedLogMessage is the exact slog message emitted for a sampled
// rate-limit rejection (design spec §9; plan correction C5).
//
// Exported so Task 8's scripts/smoke.sh log-content assertion and this
// package's own positive-control test key off the SAME literal. Step 3 of
// scripts/smoke.sh asserts the API log contains ZERO rate-limit rejections —
// a NEGATIVE grep, and a negative grep passes for two very different
// reasons: the rejection genuinely never happened, or the pattern never
// matches anything because the message text drifted from what smoke.sh's
// (necessarily hand-copied, since bash cannot import a Go constant) literal
// expects. Without a positive control proving the pattern CAN match, a typo
// on either side makes that gate permanently, silently green.
const RejectedLogMessage = "rate limit exceeded"

// Observability bundles the metrics counter and logger a rate-limited
// adapter uses to report a rejection. Bundled into one struct — rather than
// added as separate parameters on HTTP and MCP — so both adapters share one
// shape and the zero value Observability{} is always valid: any caller that
// does not care about rejection telemetry (most existing direct-construction
// tests) can pass it unmodified, and reportRejected below skips whichever
// half is unset instead of panicking on a nil field.
type Observability struct {
	Metrics *telemetry.Metrics
	Logger  *slog.Logger
}

// reportRejected records one rejection.
//
// The counter increments UNCONDITIONALLY on every rejection — it is the
// durable instrument (spec §9). The log line is written only when
// logRejection is true, at most once per key per logSampleInterval, decided
// by Limiter.AllowAtSampled/AllowSampled and by
// ConcurrencyLimiter.AcquireAtSampled/AcquireSampled, which are the only
// things allowed to compute it. A caller passing a literal true here has
// silently opted out of the sampler, which is what MCPConcurrency did.
//
// A warn per rejection would dominate log volume during exactly the incident
// this feature exists for: spec §9's own arithmetic is a client at 5k rps
// against a 50 rps limit producing ~10k lines/second, and the sampler's whole
// job is to turn that into one line a minute per key.
//
// service distinguishes the API's HTTP adapter ("api") from the gateway's
// MCP adapter ("gateway"). reason further distinguishes WHICH budget was
// exceeded: a constant for HTTP (one limiter, one budget) and the MCP method
// name for MCP, since tools/call and initialize are metered on separate
// limiters (spec §4.3). Neither is cardinality-risky — both are small closed
// sets. key (the per-principal rate-limit key) is logged but deliberately
// NEVER attached to the metric: an unbounded label value would make the
// counter itself an unbounded-cardinality time series.
func reportRejected(ctx context.Context, obs Observability, service, reason, key string, logRejection bool) {
	if obs.Metrics != nil && obs.Metrics.RateLimitRejected != nil {
		obs.Metrics.RateLimitRejected.Add(ctx, 1, metric.WithAttributes(
			attribute.String("service", service),
			attribute.String("reason", reason),
		))
	}
	if logRejection && obs.Logger != nil {
		obs.Logger.Warn(RejectedLogMessage, "service", service, "reason", reason, "key", key)
	}
}
