package api

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
)

// resolved pulls the request's ResolvedContext and Principal, both guaranteed
// present by the auth+resolve middleware. The !ok branch is defensive: if a
// resolved-only handler is ever mounted without that middleware, it fails closed
// with 500 rather than acting on a zero-value identity. Returns ok=false (after
// writing the response) when the context is missing.
func (s *Server) resolved(w http.ResponseWriter, r *http.Request) (authz.ResolvedContext, auth.Principal, bool) {
	rc, ok := authz.ResolvedFrom(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "missing resolved context")
		return authz.ResolvedContext{}, auth.Principal{}, false
	}
	p, _ := auth.PrincipalFrom(r.Context())
	return rc, p, true
}
