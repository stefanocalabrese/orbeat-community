package api

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// userDeleteResponse is handleDeleteUser's 200 body: the identity DeleteUser
// destroyed. Unlike roleDeleteResponse, this carries no cascade counts -- a
// user row has nothing downstream to REVOKE, which is a narrower claim than
// "nothing downstream" since migration 00017 (see store.DeletedUser's doc
// comment for what the delete does now destroy).
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
// infer it, but the CONTENT differs, because a user row owns no ACCESS that
// cascades: roles are reconciled from token claims per request, not from a
// stored user-role table, so deleting a user revokes nothing. So this reports
// the identity itself (subject/email/displayName), not a revoked-grants count
// like RoleDeleteResponse -- see store.DeletedUser's doc comment for the full
// reasoning, including why no SELECT ... FOR UPDATE analogous to DeleteRole's
// is needed here: there is no second table's data being read before the
// delete for a lock to protect.
//
// It does now cascade DATA, and only since migration 00017:
// artifact_deployment.user_id references users.id ON DELETE CASCADE, so this
// route is the erasure path for the deployment registry's records about one
// person. That is not the whole set, which this comment implied until audit
// finding C7: virtual_key.created_by (00020) also points at users.id, ON
// DELETE SET NULL, and since audit B33 this handler additionally REVOKES
// every virtual key the deleted user created, reporting their ids so the
// audit row can name them. The response deliberately does not count them; store.DeletedUser's
// doc comment records that as an open question rather than a closed one.
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
