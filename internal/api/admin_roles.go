package api

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

type roleInput struct {
	Name string `json:"name"`
}

type roleDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) handleCreateRole(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	var in roleInput
	if !decodeJSONOrFail(w, r, &in) {
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.checkRoleCap(r.Context(), rc.TenantID); err != nil {
		fail(w, err)
		return
	}
	var created store.Role
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		var e error
		created, e = tx.CreateRole(r.Context(), rc.TenantID, in.Name)
		if e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "role.create",
			Target: created.ID, Decision: "allow", Metadata: map[string]any{"name": created.Name},
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, roleDTO{ID: created.ID, Name: created.Name})
}

// handleListRoles is keyset-paginated (?limit, ?cursor; see paging.go). The
// returned nextCursor uses the standard "possibly more" heuristic:
// len(rows)==limit means maybe-more, never definitely-more (there is no
// LIMIT+1 lookahead), so a tenant whose true role count is an exact multiple
// of limit gets one extra page back empty with nextCursor=="". That is
// expected keyset-pagination behavior, not a bug — pinned by
// TestListRolesPaginationExactMultiplePage in paging_test.go.
func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	limit, cursor, err := pageParams(r, defaultListLimit, maxListLimit, cursorShape{cursorText})
	if err != nil {
		fail(w, err)
		return
	}
	roles, err := s.store.ListRolesPage(r.Context(), rc.TenantID, cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]roleDTO, 0, len(roles))
	for _, role := range roles {
		out = append(out, roleDTO{ID: role.ID, Name: role.Name})
	}
	next := ""
	if len(roles) == limit && limit > 0 {
		next = encodeListCursor(store.RoleCursor(roles[len(roles)-1]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": out, "limit": limit, "nextCursor": next})
}

// roleDeleteResponse is handleDeleteRole's 200 body: the counts of what its
// cascade revoked. Both fields are always present (never omitted), even when
// zero — a role deleted with no grants still returns {0, 0}, not an absent
// key, so a client never has to special-case "field missing" vs "zero".
type roleDeleteResponse struct {
	EntitlementsRevoked         int `json:"entitlementsRevoked"`
	ArtifactEntitlementsRevoked int `json:"artifactEntitlementsRevoked"`
}

// handleDeleteRole removes a role and everything granted to it.
//
// Deleting a role cascades to entitlement and artifact_entitlement (ON DELETE
// CASCADE, two FK paths each — see store.DeleteRole's doc comment), so this
// one call revokes every server and artifact grant hung off the role. That is
// intended (docs/specs/2026-08-11-orbeat-role-deletion-design.md §3.1): the
// protection here is legibility, not prevention. The audit metadata below
// names exactly what was revoked so an operator can later answer "why did
// alice lose access?" and re-grant it if the deletion was a mistake —
// recoverable by inspection, NOT reversible. There is no undo.
//
// Returns 200 with the revoked counts rather than 204 like its sibling
// deletes (handleDeleteServer, handleDeleteEntitlement — spec §6.1): the
// portal cannot compute them client-side, because useEntitlements and
// useArtifactEntitlements are capped at 100 rows by default, and a
// client-side count would silently understate the blast radius on exactly
// the roles that have the most grants — the ones where getting it wrong
// matters most.
func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var revoked store.RevokedGrants
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		g, e := tx.DeleteRole(r.Context(), rc.TenantID, id)
		if e != nil {
			return store.AuditEvent{}, e
		}
		revoked = g
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "role.delete",
			Target: id, Decision: "allow",
			Metadata: map[string]any{
				"name":                        g.RoleName,
				"entitlementsRevoked":         g.Entitlements,
				"artifactEntitlementsRevoked": g.ArtifactEntitlements,
				"servers":                     g.ServerNames,
				"artifacts":                   g.ArtifactNames,
				"truncated":                   g.Truncated,
			},
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roleDeleteResponse{
		EntitlementsRevoked:         revoked.Entitlements,
		ArtifactEntitlementsRevoked: revoked.ArtifactEntitlements,
	})
}
