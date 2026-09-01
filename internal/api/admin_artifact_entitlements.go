package api

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

type artifactEntitlementInput struct {
	RoleID     string `json:"roleId"`
	ArtifactID string `json:"artifactId"`
}

type artifactEntitlementDTO struct {
	ID         string `json:"id"`
	RoleID     string `json:"roleId"`
	ArtifactID string `json:"artifactId"`
}

func toArtifactEntitlementDTO(e store.ArtifactEntitlement) artifactEntitlementDTO {
	return artifactEntitlementDTO{ID: e.ID, RoleID: e.RoleID, ArtifactID: e.ArtifactID}
}

func (s *Server) handleCreateArtifactEntitlement(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	var in artifactEntitlementInput
	if !decodeJSONOrFail(w, r, &in) {
		return
	}
	if in.RoleID == "" || in.ArtifactID == "" {
		writeError(w, http.StatusBadRequest, "roleId and artifactId are required")
		return
	}
	var created store.ArtifactEntitlement
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		// Tenant + existence checks inside the tx (FOR SHARE locks prevent a
		// concurrent delete from racing the insert; FKs alone don't enforce tenant).
		roleOK, e := tx.RoleExistsInTenant(r.Context(), rc.TenantID, in.RoleID)
		if e != nil {
			return store.AuditEvent{}, e
		}
		if !roleOK {
			return store.AuditEvent{}, validationError{"unknown roleId for tenant"}
		}
		artOK, e := tx.ArtifactExistsInTenant(r.Context(), rc.TenantID, in.ArtifactID)
		if e != nil {
			return store.AuditEvent{}, e
		}
		if !artOK {
			return store.AuditEvent{}, validationError{"unknown artifactId for tenant"}
		}
		created, e = tx.CreateArtifactEntitlement(r.Context(), store.ArtifactEntitlement{
			TenantID: rc.TenantID, RoleID: in.RoleID, ArtifactID: in.ArtifactID,
		})
		if e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "artifact_entitlement.create",
			Target: created.ID, Decision: "allow",
			Metadata: map[string]any{"roleId": created.RoleID, "artifactId": created.ArtifactID},
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toArtifactEntitlementDTO(created))
}

// handleListArtifactEntitlements is keyset-paginated (?limit, ?cursor; see
// paging.go), cursored on role_id — NOT a unique column, mirroring
// handleListEntitlements above (see its comment for why the (role_id, id)
// tiebreaker matters). The nextCursor heuristic (len(rows)==limit means
// "possibly more"; an exact multiple of limit costs one extra empty page) is
// documented once, on handleListRoles.
//
// ?q= is REFUSED with 400, mirroring handleListEntitlements above for the
// identical reason (Decision 1, docs/plans/orbeat-admin-search-sort-2026-08-27.md
// Task 4): role_id is a uuid with no natural text column of its own.
func (s *Server) handleListArtifactEntitlements(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	limit, cursor, err := pageParams(r, defaultListLimit, maxListLimit, cursorShape{cursorUUID})
	if err != nil {
		fail(w, err)
		return
	}
	if err := refuseSearch(r); err != nil {
		fail(w, err)
		return
	}
	desc, err := sortOrderParams(r, artifactEntitlementSortName)
	if err != nil {
		fail(w, err)
		return
	}
	ents, err := s.store.ListArtifactEntitlementsPage(r.Context(), rc.TenantID, cursor, limit, desc)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]artifactEntitlementDTO, 0, len(ents))
	for _, e := range ents {
		out = append(out, toArtifactEntitlementDTO(e))
	}
	next := ""
	if len(ents) == limit && limit > 0 {
		next = encodeListCursor(store.ArtifactEntitlementCursor(ents[len(ents)-1], desc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifactEntitlements": out, "limit": limit, "nextCursor": next})
}

func (s *Server) handleDeleteArtifactEntitlement(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		if e := tx.DeleteArtifactEntitlement(r.Context(), rc.TenantID, id); e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "artifact_entitlement.delete",
			Target: id, Decision: "allow",
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
