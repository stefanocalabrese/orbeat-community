package ratelimit

import "time"

// logSampleInterval bounds how often ONE key may produce a sampled rejection
// log line: at most one per key per interval, whatever shape the traffic takes.
//
// It replaces a per-streak flag that an allowed request cleared. That flag was
// justified by the claim that "the token bucket already guarantees no allowed
// request lands between two rejections until the client's own rate actually
// drops", which is false: rate.Limiter refills continuously, so a client above
// the limit is admitted once every 1/rps seconds and each admission cleared the
// flag. Measured against the gateway's shipped rps=20 burst=60 with a 25 rps
// client for 60 seconds, 241 rejections produced 241 log lines. A wall-clock
// interval has no traffic shape that defeats it, which a flag keyed on
// "something else happened" always will.
//
// ONE MINUTE, and the reasoning is a bound rather than a taste:
//
//   - It costs nothing in detection latency. The first rejection after any
//     quiet interval still logs immediately (sampleLog is true on a zero
//     lastLogged and after the interval elapses), so an operator alerting on
//     this warn learns about a new incident exactly as fast as before. The
//     interval governs only the heartbeat while an incident CONTINUES.
//   - The bound it buys is hard. One line per key per minute, against the
//     241/min measured above for a single key barely over the limit, and
//     against spec §9's own worst case of a 5000 rps client on a 50 rps budget,
//     which is 4950 rejections a second, or ~297000 lines a minute, from one
//     key.
//   - It is the per-key bound multiplied by the key map's own ceiling that
//     decides the real worst case, and that is what argues against a shorter
//     interval: Limiter's maxEntries is 10000 in both binaries, so a minute
//     caps the process at 10000 lines/minute (167/s) even with every bucket
//     simultaneously over its limit, where ten seconds would allow 1000/s,
//     back inside the flood regime this exists to prevent.
//   - It is comfortably inside the smallest bucket TTL either binary ships
//     (the API's 5 minutes; the gateway's is 10), which matters because an
//     evicted bucket loses its lastLogged and its next rejection logs again.
//     At an interval at or above the TTL, eviction rather than this constant
//     would be setting the rate, and a sustained incident's heartbeat would
//     arrive no more often than a bucket's whole lifetime.
//
// The honest residual: the bound is one line per key per interval PLUS one per
// bucket re-creation, since a swept or LRU-evicted bucket starts with a zero
// lastLogged. That is bounded by key churn, not by rejection rate, so no client
// can inflate it by sending faster.
//
// The rejection COUNTER is unsampled and unaffected. It is the durable
// instrument; the log line is a breadcrumb pointing at which key.
const logSampleInterval = time.Minute

// sampleLog reports whether a rejection observed at now should be logged for a
// key whose last sampled line was written at lastLogged, the zero value when
// none ever was.
//
// The zero case is tested explicitly rather than left to arithmetic. Sub
// saturates at the maximum Duration for a gap this large, so the comparison
// would in fact be true, but relying on a saturation rule to express "this has
// never been logged" is a reading the next change could quietly break.
//
// Callers MUST store now into lastLogged only when this returns true. Stamping
// it on every rejection means the interval never elapses under sustained
// overload and only the very first line of an incident is ever written, which
// is a silent sampler rather than a noisy one and is worse.
func sampleLog(lastLogged, now time.Time) bool {
	return lastLogged.IsZero() || now.Sub(lastLogged) >= logSampleInterval
}
