package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// Metrics holds orbeat's application metric instruments. Built from a meter, so
// all instruments are inert when telemetry is disabled (the global meter is then
// a no-op) — callers increment unconditionally. Governance counters give the
// domain signal; RBAC decisions come from the gateway.
type Metrics struct {
	ArtifactSubmit    metric.Int64Counter
	ArtifactApprove   metric.Int64Counter
	ArtifactReject    metric.Int64Counter
	ArtifactRollback  metric.Int64Counter
	RBACDecision      metric.Int64Counter
	RateLimitRejected metric.Int64Counter
	// SessionsReclaimed counts gateway sessions evicted from our own cache,
	// tagged with a "cause" attribute drawn from a small closed set. Do not
	// re-list the causes here: an enumeration in this file drifted to four
	// while session.go defined six (audit C9), and a reader summing a
	// four-member set against a six-member metric gets a number that is
	// simply wrong. internal/gateway/session.go's reclaim* constants are the
	// set. The cache was described here as "subject-keyed" until audit B14
	// keyed it on subject and azp together, which falsified this comment two
	// packages away from the change. It says nothing about the SDK's own
	// transport-session map, which this process cannot observe from here
	// (gateway session lifecycle design, 2026-08-16 §6).
	SessionsReclaimed metric.Int64Counter
	// UpstreamConnect measures how long dialing one upstream MCP server takes —
	// DNS, TCP, TLS and the MCP handshake together, which is what an operator
	// pays when a session includes that server. Attributed by server name
	// (bounded by the catalog) and outcome.
	//
	// Explicit boundaries in SECONDS. The SDK's defaults run 0..10000, tuned for
	// milliseconds (sdk/metric reader.go), so with a seconds unit every dial —
	// capped at upstreamDialTimeout, 10s — would land in the first bucket and the
	// histogram would distinguish nothing. The top boundary matches that timeout:
	// the watchdog cancels first, so nothing can land above it.
	UpstreamConnect metric.Float64Histogram
	// SessionLookup counts session-cache lookups by result (hit|miss). A hit
	// ratio is computed at query time; storing a ratio would be wrong.
	SessionLookup metric.Int64Counter
	// PinnedPayloadRaceFallback counts GET /v1/sync/artifacts' payload-race
	// fallback (internal/api/sync.go, resolveSyncPayload): the revision window
	// is read by one query and a resolved pin's payload by a second, so an
	// approval landing between them can prune the exact revision the clamp
	// just resolved. The handler still serves the approved snapshot and
	// reports "pruned" either way; this counter is what makes an otherwise
	// invisible path something an operator can see and alert on. Enterprise
	// only in practice (Server.pinning gates the whole ?pin path a Community
	// build never enters), but the instrument itself is edition-agnostic, the
	// same shape as every counter above.
	PinnedPayloadRaceFallback metric.Int64Counter
}

// NewMetrics builds the instruments from the given meter. Pass otel.Meter(scope)
// in production; pass a manual-reader meter in tests.
func NewMetrics(m metric.Meter) *Metrics {
	submit, _ := m.Int64Counter("orbeat.artifact.submit", metric.WithDescription("artifact submissions"))
	approve, _ := m.Int64Counter("orbeat.artifact.approve", metric.WithDescription("artifact approvals"))
	reject, _ := m.Int64Counter("orbeat.artifact.reject", metric.WithDescription("artifact rejections"))
	rollback, _ := m.Int64Counter("orbeat.artifact.rollback", metric.WithDescription("artifact rollbacks"))
	rbac, _ := m.Int64Counter("orbeat.gateway.rbac_decisions", metric.WithDescription("gateway RBAC decisions"))
	// Dotted OTel naming with NO "_total" suffix, matching every sibling
	// counter above: the Prometheus exporter appends "_total" itself, so a
	// literal "orbeat_ratelimit_rejected_total" here would export as
	// "..._total_total" and sort away from its siblings.
	ratelimited, _ := m.Int64Counter("orbeat.ratelimit.rejected", metric.WithDescription("rate-limited requests"))
	reclaimed, _ := m.Int64Counter("orbeat.gateway.sessions.reclaimed", metric.WithDescription("gateway sessions reclaimed from our per-principal cache, by cause"))
	connect, _ := m.Float64Histogram("orbeat.gateway.upstream.connect.duration",
		metric.WithDescription("time to dial one upstream MCP server, by server and outcome"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	lookup, _ := m.Int64Counter("orbeat.gateway.sessions.lookup", metric.WithDescription("gateway session-cache lookups, by result"))
	pinRace, _ := m.Int64Counter("orbeat.sync.pin.payload_race_fallback",
		metric.WithDescription("GET /v1/sync/artifacts payload-race fallbacks: a clamp-resolved pinned revision was pruned before its payload could be read"))
	return &Metrics{
		ArtifactSubmit: submit, ArtifactApprove: approve, ArtifactReject: reject, ArtifactRollback: rollback,
		RBACDecision: rbac, RateLimitRejected: ratelimited, SessionsReclaimed: reclaimed,
		UpstreamConnect: connect, SessionLookup: lookup, PinnedPayloadRaceFallback: pinRace,
	}
}

// RegisterPoolGauges registers observable gauges reporting pgxpool stats on the
// given meter. inUse/idle are read at collection time.
func RegisterPoolGauges(m metric.Meter, inUse, idle func() int64) error {
	if _, err := m.Int64ObservableGauge("orbeat.db.pool.in_use",
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error { o.Observe(inUse()); return nil })); err != nil {
		return err
	}
	_, err := m.Int64ObservableGauge("orbeat.db.pool.idle",
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error { o.Observe(idle()); return nil }))
	return err
}

// RegisterSessionGauge registers an observable gauge reporting the size of
// the gateway's own per-principal session cache, read at collection time.
// This is NOT the SDK's separate transport-session map (keyed by MCP session
// id, with its own independent lifecycle — gateway session lifecycle design,
// 2026-08-16 §2); count may include entries that are logically expired but
// not yet swept by the background reaper, matching what the cache itself
// currently holds.
func RegisterSessionGauge(m metric.Meter, count func() int64) error {
	_, err := m.Int64ObservableGauge("orbeat.gateway.sessions.live",
		metric.WithDescription("live gateway sessions in the per-principal cache"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error { o.Observe(count()); return nil }))
	return err
}
