package authz

import (
	"context"
	"errors"
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
)

type resolvedCtxKey struct{}

// WithResolved stores rc in ctx.
func WithResolved(ctx context.Context, rc ResolvedContext) context.Context {
	return context.WithValue(ctx, resolvedCtxKey{}, rc)
}

// ResolvedFrom returns the ResolvedContext stored in ctx, if any.
func ResolvedFrom(ctx context.Context) (ResolvedContext, bool) {
	rc, ok := ctx.Value(resolvedCtxKey{}).(ResolvedContext)
	return rc, ok
}

// Middleware resolves the validated Principal to its database context once per
// request and injects it. MUST be mounted after auth.RequireAuth. Missing
// principal → 401 (fail closed); a SeatLimitError (Community edition seat cap,
// spec §4/§5) → 402 with the same structured body internal/api's cap
// responses use; a DeactivatedUserError (SCIM deprovisioning, migration
// 00021) → 403, for the reason its branch below records; any other resolve
// error → 500.
func (r *Resolver) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p, ok := auth.PrincipalFrom(req.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rc, err := r.Resolve(req.Context(), p)
		if err != nil {
			var sErr SeatLimitError
			if errors.As(err, &sErr) {
				writeSeatLimitReached(w, sErr)
				return
			}
			// A deactivated user is a DECISION, not a failure. Without this
			// branch they fall through to the 500 below, so SCIM
			// deprovisioning works but looks like an outage: the operator who
			// just deactivated someone sees "internal error" and reasonably
			// concludes orbeat is broken rather than that it obeyed them.
			// 403 rather than 401, because the token is valid and
			// re-authenticating would change nothing.
			var dErr DeactivatedUserError
			if errors.As(err, &dErr) {
				http.Error(w, "forbidden: user deactivated", http.StatusForbidden)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, req.WithContext(WithResolved(req.Context(), rc)))
	})
}
