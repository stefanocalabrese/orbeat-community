package authz

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
)

// RequireRole returns middleware that allows the request only if the validated
// Principal carries the named role. It fails closed: missing principal or role
// yields 403 and the wrapped handler is not called. Use it AFTER auth.RequireAuth.
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.PrincipalFrom(r.Context())
			if !ok || !hasRole(p.Roles, role) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyRole is RequireRole for a set: the request proceeds if the
// Principal carries AT LEAST ONE of the named roles. Empty names are ignored,
// which is what lets an edition-specific role be passed in as "" and simply not
// widen the gate (see api.artifactManagerRole).
//
// It fails closed on the same two conditions RequireRole does, and for the same
// reason it must stay cheap: it runs BEFORE the resolver, so a caller with no
// privileged role never pays for the tenant/user lookup.
func RequireAnyRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.PrincipalFrom(r.Context())
			if !ok || !hasAnyRole(p.Roles, roles) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func hasAnyRole(have []string, want []string) bool {
	for _, w := range want {
		if w == "" {
			continue
		}
		if hasRole(have, w) {
			return true
		}
	}
	return false
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}
