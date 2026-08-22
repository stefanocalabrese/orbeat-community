package api

import (
	"context"
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
)

// identityCtxKey carries a *requestIdentity through the handler chain
// installed by withIdentityCarrier.
type identityCtxKey struct{}

// requestIdentity is a mutable, request-scoped accumulator for whatever of the
// caller's identity this request's own processing resolves.
//
// It exists because the obvious wiring does not work, and shipped not working.
// logging.Requests captures its own ctx variable BEFORE calling into the rest
// of the chain and reads it again only AFTER that call returns. context.Context
// is immutable, so resolver.Middleware's context.WithValue produces a NEW
// context visible only further down that same call, never to the ancestor's
// already-captured variable. apiIdentity therefore read a context that could
// never contain a resolved identity, and returned ("", "") on every request
// since the day it was written.
//
// Measured before the fix: of 85 status=200 request-log lines emitted across
// the internal/api suite, every one of them through resolver.Middleware, ZERO
// carried a subject field. The doc comment claimed the opposite.
//
// Only mutating the SAME pointee crosses that boundary. Installing a pointer
// here, before logging.Requests runs, means any handler downstream (however
// many context.WithValue layers deep) retrieves and mutates the same
// *requestIdentity, and logging.Requests' earlier snapshot observes it.
//
// The alternative, moving logging.Requests to run after auth, was rejected for
// the same reason internal/gateway rejected it: RequireAuth returns without
// calling its wrapped handler on a bad token, so nesting the logger inside it
// would silence the request line for every 401, the case the line is most
// useful for.
type requestIdentity struct {
	tenant  string
	subject string
}

// withIdentityCarrier installs an empty *requestIdentity before the rest of the
// chain runs. Must be mounted OUTSIDE logging.Requests so that logger's own ctx
// snapshot already holds the carrier pointer.
func withIdentityCarrier(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityCtxKey{}, &requestIdentity{})))
	})
}

// recordIdentity copies whatever identity is present at its position in the
// chain into the carrier. It is deliberately placed differently per route
// class, and records only what is genuinely known there rather than filling
// both fields for symmetry:
//
//   - authed/admin routes mount it INSIDE resolver.Middleware, so both the
//     token subject and the resolved tenant are available.
//   - GET /v1/me does no DB resolve at all, so it mounts it after RequireAuth
//     and yields a subject with an empty tenant. Empty is the truthful value;
//     a fabricated tenant would be worse than an absent one.
//
// A no-op when ctx carries no carrier, so handlers stay directly exercisable in
// tests that do not go through Handler().
func recordIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if id, ok := ctx.Value(identityCtxKey{}).(*requestIdentity); ok {
			if p, ok := auth.PrincipalFrom(ctx); ok {
				id.subject = p.Subject
			}
			if rc, ok := authz.ResolvedFrom(ctx); ok {
				id.tenant = rc.TenantID
				// Prefer the resolved user id: it is this deployment's own
				// identifier for the caller, whereas the token subject is the
				// IdP's. On authed/admin routes both exist and they differ.
				if rc.UserID != "" {
					id.subject = rc.UserID
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// apiIdentity is logging.Requests' identity callback. It reads the carrier
// rather than the resolved context directly: see requestIdentity's comment for
// why reading authz.ResolvedFrom here cannot work.
func apiIdentity(ctx context.Context) (tenant, subject string) {
	id, ok := ctx.Value(identityCtxKey{}).(*requestIdentity)
	if !ok {
		return "", ""
	}
	return id.tenant, id.subject
}
