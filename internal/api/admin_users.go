package api

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// userDeleteResponse is handleDeleteUser's 200 body: the identity DeleteUser
// destroyed. Unlike roleDeleteResponse, this carries no cascade counts -- a
// user row has nothing downstream to revoke (see store.DeletedUser's doc
// comment for why).
type userDeleteResponse struct {
	Subject     string `json:"subject"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

// handleDeleteUser removes a user row.
//
// There is no create/update handler for users: a row is populated only by
// UpsertUser, called from authz.Resolver.Resolve on every authenticated
// request. This route exists to release the identity, not to manage it.
//
// Returns 200 with the deleted identity rather than 204 like its sibling
// deletes for servers/entitlements: mirrors handleDeleteRole's choice
// (v1.24.0) of reporting what was destroyed rather than leaving the caller to
// infer it, but the CONTENT differs, because a user row owns nothing that
// cascades -- no table in the schema holds a foreign key to users.id (roles
// are reconciled from token claims per request, not a stored user-role
// table). So this reports the identity itself (subject/email/displayName),
// not a revoked-grants count like RoleDeleteResponse -- see
// store.DeletedUser's doc comment for the full reasoning, including why no
// SELECT ... FOR UPDATE analogous to DeleteRole's is needed here: there is no
// second table's data being read before the delete for a lock to protect.
//
// Deletion is not a ban (spec docs/specs/2026-08-19-orbeat-community-caps-
// design.md sec 3.3): a deleted subject who authenticates again is upserted
// afresh by the very next request and consumes a Community seat again if one
// is free.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var deleted store.DeletedUser
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		d, e := tx.DeleteUser(r.Context(), rc.TenantID, id)
		if e != nil {
			return store.AuditEvent{}, e
		}
		deleted = d
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "user.delete",
			Target: id, Decision: "allow",
			Metadata: map[string]any{
				"subject":     d.Subject,
				"email":       d.Email,
				"displayName": d.DisplayName,
			},
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, userDeleteResponse{
		Subject:     deleted.Subject,
		Email:       deleted.Email,
		DisplayName: deleted.DisplayName,
	})
}
