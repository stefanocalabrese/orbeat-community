package api

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
)

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"subject": p.Subject, "email": p.Email, "roles": p.Roles})
}
