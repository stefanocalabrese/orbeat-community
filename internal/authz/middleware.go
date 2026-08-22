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

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}
