package ratelimit

import (
	"math"
	"net/http"
	"strconv"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
)

// HTTP rejects requests whose principal has exhausted its bucket.
//
// Placed AFTER RequireAuth (it needs a principal) and BEFORE
// resolver.Middleware (so a rejected request never pays the tenant/user
// lookup) — the same reasoning that put RequireRole before the resolver
// (internal/api/api.go's admin closure).
//
// A request with no principal in context (should not happen on a route this
// is wired into, since RequireAuth always runs first) passes through
// unlimited rather than panicking — fail open on a wiring mistake rather than
// 500ing every request.
//
// Retry-After is integer delta-seconds per RFC 9110, rounded UP with a floor
// of 1: emitting 0 invites an immediate retry from a client already being
// throttled.
//
// obs (Task 7, spec §9) reports every rejection to the ratelimit.rejected
// counter and, at most once per key per streak, a sampled log breadcrumb.
// Its zero value is safe, so callers that do not care about this
// telemetry (most direct-construction tests) can pass Observability{}.
func HTTP(l *Limiter, deny func(http.ResponseWriter, *http.Request), obs Observability, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		key := KeyFor(p)
		allowed, retry, logRejection := l.AllowSampled(key)
		if !allowed {
			secs := int(math.Ceil(retry.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			reportRejected(r.Context(), obs, "api", "http", key, logRejection)
			deny(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
