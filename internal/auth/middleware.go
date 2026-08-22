package auth

import (
	"net/http"
	"strings"
)

// RequireAuth wraps next, requiring a valid Bearer access token. On any failure
// it responds 401 with a WWW-Authenticate challenge and does NOT call next
// (fail closed). On success it injects the Principal into the request context.
func (v *Validator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			challenge(w, "missing bearer token")
			return
		}
		p, err := v.Validate(r.Context(), raw)
		if err != nil {
			challenge(w, "invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

func challenge(w http.ResponseWriter, desc string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="`+desc+`"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
