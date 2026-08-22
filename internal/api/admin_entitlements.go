package api

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

type entitlementInput struct {
	RoleID       string   `json:"roleId"`
	MCPServerID  string   `json:"mcpServerId"`
	AllowedTools []string `json:"allowedTools"` // nil = all tools
	Permissions  []string `json:"permissions"`
}

type entitlementDTO struct {
	ID           string   `json:"id"`
	RoleID       string   `json:"roleId"`
	MCPServerID  string   `json:"mcpServerId"`
	AllowedTools []string `json:"allowedTools"`
	Permissions  []string `json:"permissions"`
}

func toEntitlementDTO(e store.Entitlement) entitlementDTO {
	return entitlementDTO{
		ID: e.ID, RoleID: e.RoleID, MCPServerID: e.MCPServerID,
		AllowedTools: e.AllowedTools, Permissions: e.Permissions,
	}
}

func (s *Server) handleCreateEntitlement(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	var in entitlementInput
	if !decodeJSONOrFail(w, r, &in) {
		return
	}
	if in.RoleID == "" || in.MCPServerID == "" {
		writeError(w, http.StatusBadRequest, "roleId and mcpServerId are required")
		return
	}
	var created store.Entitlement
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		// Referential + tenant check inside the tx: both the role and server must
		// belong to the caller's tenant. FOR SHARE locks the referenced rows so a
		// concurrent delete can't race the insert (DB FKs enforce existence but
		// NOT tenant match).
		roleOK, e := tx.RoleExistsInTenant(r.Context(), rc.TenantID, in.RoleID)
		if e != nil {
			return store.AuditEvent{}, e
		}
		if !roleOK {
			return store.AuditEvent{}, validationError{"unknown roleId for tenant"}
		}
		serverOK, e := tx.MCPServerExistsInTenant(r.Context(), rc.TenantID, in.MCPServerID)
		if e != nil {
			return store.AuditEvent{}, e
		}
		if !serverOK {
			return store.AuditEvent{}, validationError{"unknown mcpServerId for tenant"}
		}
		created, e = tx.CreateEntitlement(r.Context(), store.Entitlement{
			TenantID: rc.TenantID, RoleID: in.RoleID, MCPServerID: in.MCPServerID,
			AllowedTools: in.AllowedTools, Permissions: in.Permissions,
		})
		if e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "entitlement.create",
			Target: created.ID, Decision: "allow",
			Metadata: map[string]any{"roleId": created.RoleID, "mcpServerId": created.MCPServerID},
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toEntitlementDTO(created))
}

// handleListEntitlements is keyset-paginated (?limit, ?cursor; see
// paging.go), cursored on role_id — NOT a unique column (a role can have many
// entitlements), which is exactly why the (role_id, id) tiebreaker exists in
// store.EntitlementCursor/ListEntitlementsPage: without it, a page boundary
// landing mid-role would silently skip or duplicate rows. The nextCursor
// heuristic (len(rows)==limit means "possibly more"; an exact multiple of
// limit costs one extra empty page) is documented once, on handleListRoles.
func (s *Server) handleListEntitlements(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	limit, cursor, err := pageParams(r, defaultListLimit, maxListLimit, cursorShape{cursorUUID})
	if err != nil {
		fail(w, err)
		return
	}
	ents, err := s.store.ListEntitlementsPage(r.Context(), rc.TenantID, cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]entitlementDTO, 0, len(ents))
	for _, e := range ents {
		out = append(out, toEntitlementDTO(e))
	}
	next := ""
	if len(ents) == limit && limit > 0 {
		next = encodeListCursor(store.EntitlementCursor(ents[len(ents)-1]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"entitlements": out, "limit": limit, "nextCursor": next})
}

func (s *Server) handleDeleteEntitlement(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	err := s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		if e := tx.DeleteEntitlement(r.Context(), rc.TenantID, id); e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "entitlement.delete",
			Target: id, Decision: "allow",
		}, nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
