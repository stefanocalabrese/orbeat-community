package api

import (
	"errors"
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
	// RowVersion is the optimistic-concurrency token a PUT must echo in
	// If-Match. Carried on the LIST rows too, not only the by-id read, so the
	// admin console can edit a grant straight from the table it is already
	// looking at, the same way it edits a server.
	RowVersion int64 `json:"rowVersion"`
}

func toEntitlementDTO(e store.Entitlement) entitlementDTO {
	return entitlementDTO{
		ID: e.ID, RoleID: e.RoleID, MCPServerID: e.MCPServerID,
		AllowedTools: e.AllowedTools, Permissions: e.Permissions,
		RowVersion: e.RowVersion,
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
//
// ?q= is REFUSED with 400 (Decision 1, docs/plans/orbeat-admin-search-sort-
// 2026-08-27.md Task 4, see refuseSearch's own comment, paging.go, and
// entitlementKeys' comment in internal/store/rbac.go for the full reasoning):
// role_id is a uuid with no natural text column of its own to search.
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
	if err := refuseSearch(r); err != nil {
		fail(w, err)
		return
	}
	desc, err := sortOrderParams(r, entitlementSortName)
	if err != nil {
		fail(w, err)
		return
	}
	ents, err := s.store.ListEntitlementsPage(r.Context(), rc.TenantID, cursor, limit, desc)
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
		next = encodeListCursor(store.EntitlementCursor(ents[len(ents)-1], desc))
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

// handleGetEntitlement is where a client obtains the ETag it must echo back in
// If-Match to update this grant. The list route deliberately does not carry
// one: an ETag on a collection would describe the page, not any row in it.
func (s *Server) handleGetEntitlement(w http.ResponseWriter, r *http.Request) {
	rc, _, ok := s.resolved(w, r)
	if !ok {
		return
	}
	e, err := s.store.GetEntitlement(r.Context(), rc.TenantID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("ETag", etag(e.RowVersion))
	writeJSON(w, http.StatusOK, toEntitlementDTO(e))
}

// handleUpdateEntitlement full-replaces a grant's allowedTools and permissions.
//
// If-Match is REQUIRED, for the reason v1.23.0 established and this route
// inherits rather than re-argues: a full-replace update without it lets two
// admins silently overwrite each other, and what is being overwritten here is
// the set of tools a role may call. `roleId` and `mcpServerId` are NOT
// updatable (see store.UpdateEntitlement): repointing a grant at a different
// role or server is a revoke plus a grant, and doing it under an "update" audit
// action would move access between principals while the trail says an edit
// happened.
func (s *Server) handleUpdateEntitlement(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	// Parsed before any read or write: a missing, malformed or refused
	// precondition must reject the request without touching the store.
	expected, err := ifMatch(r)
	if err != nil {
		fail(w, err)
		return
	}
	id := r.PathValue("id")
	var in entitlementInput
	if !decodeJSONOrFail(w, r, &in) {
		return
	}
	var updated store.Entitlement
	err = s.auditedTx(r.Context(), func(tx *store.Store) (store.AuditEvent, error) {
		var e error
		updated, e = tx.UpdateEntitlement(r.Context(), store.Entitlement{
			ID: id, TenantID: rc.TenantID,
			AllowedTools: in.AllowedTools, Permissions: in.Permissions,
			RowVersion: expected,
		})
		if e != nil {
			return store.AuditEvent{}, e
		}
		return store.AuditEvent{
			TenantID: rc.TenantID, Actor: p.Subject, Action: "entitlement.update",
			Target: updated.ID, Decision: "allow",
			Metadata: map[string]any{
				"roleId": updated.RoleID, "mcpServerId": updated.MCPServerID,
				"allowedTools": updated.AllowedTools,
			},
		}, nil
	})
	if err != nil {
		// A stale If-Match is a REJECTED MUTATION on an authorization surface,
		// so it leaves a durable trace before the client sees the 412, exactly
		// as handleUpdateServer does. A 428 (missing If-Match) is a client bug
		// rather than a security event and is deliberately not audited.
		if errors.Is(err, store.ErrVersionMismatch) {
			if aerr := s.appendDenyAudit(r.Context(), store.AuditEvent{
				TenantID: rc.TenantID, Actor: p.Subject, Action: "entitlement.update",
				Target: id, Decision: "deny",
				Metadata: map[string]any{"reason": "version_mismatch"},
			}); aerr != nil {
				fail(w, aerr)
				return
			}
		}
		fail(w, err)
		return
	}
	w.Header().Set("ETag", etag(updated.RowVersion))
	writeJSON(w, http.StatusOK, toEntitlementDTO(updated))
}
