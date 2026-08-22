package gateway

import (
	"context"
	"net/http"
)

// identityCtxKey carries a *requestIdentity through the handler chain
// installed by withIdentityCarrier.
type identityCtxKey struct{}

// requestIdentity is a mutable, request-scoped accumulator for whatever piece
// of the caller's OAuth identity this request's own processing manages to
// resolve, filled in by withSession as it learns each piece.
//
// It exists because of a context.Context constraint that also affects
// internal/api's own identity wiring: logging.Requests captures its own ctx
// variable BEFORE calling into the rest of the handler chain, and reads it
// again only AFTER that call returns. context.Context is immutable — a
// downstream middleware's context.WithValue additions produce a NEW context
// object visible only to code further down that same call, never to an
// ancestor's already-captured ctx variable (verified against this exact
// shape before wiring it in: an outer wrapper reading ctx.Value(k) after
// calling next(), where next() adds k via a plain context.WithValue, sees
// nothing). Only mutating the SAME pointee crosses that boundary, which is
// what installing a pointer here — before logging.Requests runs — buys: any
// handler downstream, no matter how many context.WithValue layers deep,
// retrieves and mutates the SAME *requestIdentity, and logging.Requests's own
// (earlier) ctx snapshot observes the mutation through that shared pointer.
//
// The alternative — moving logging.Requests to run only after auth/session
// resolution — was rejected: RequireBearerToken (go-sdk) returns before
// calling its wrapped handler on an invalid/missing token, so nesting
// logging.Requests inside it would silence the request-log line entirely for
// every 401, the one failure mode this log line is most useful for. This
// carrier keeps logging.Requests at its original position (wrapping the
// whole mux, exactly like internal/api's Handler()), so every request is
// still logged exactly once, with whatever identity was truthfully resolved
// by the time it returns — empty fields, not fabricated ones, when nothing
// was resolved.
type requestIdentity struct {
	tenant  string
	subject string
}

// withIdentityCarrier installs an empty *requestIdentity into ctx before the
// rest of the chain runs. Must be mounted OUTSIDE (upstream of)
// logging.Requests, so logging.Requests's own ctx snapshot already contains
// the carrier pointer — see requestIdentity's doc comment.
func withIdentityCarrier(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := &requestIdentity{}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, id)))
	})
}

// recordSubject and recordTenant are called by withSession as soon as it
// learns each piece of identity — recordSubject once the bearer token's
// Principal is recovered (true even if session build subsequently fails),
// recordTenant once a session (cached or freshly built) yields a resolved
// tenant. Both are no-ops when ctx carries no carrier, so withSession stays
// safe to exercise directly in tests that build a Server without going
// through Handler()'s withIdentityCarrier.
func recordSubject(ctx context.Context, subject string) {
	if id, ok := ctx.Value(identityCtxKey{}).(*requestIdentity); ok {
		id.subject = subject
	}
}

func recordTenant(ctx context.Context, tenant string) {
	if id, ok := ctx.Value(identityCtxKey{}).(*requestIdentity); ok {
		id.tenant = tenant
	}
}

// gatewayIdentity is logging.Requests' identity callback for the gateway
// (Handler() wires it in place of the prior nil). Tenant is only known once
// withSession has resolved (or cache-fetched) the caller's session, so on the
// no-principal (401) and session-build-failure (503) paths it is genuinely
// empty — not a guess, exactly what this request's own processing learned.
func gatewayIdentity(ctx context.Context) (tenant, subject string) {
	id, ok := ctx.Value(identityCtxKey{}).(*requestIdentity)
	if !ok {
		return "", ""
	}
	return id.tenant, id.subject
}
