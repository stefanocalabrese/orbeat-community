package api

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/rbac"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// serverDTO is the catalog projection. It intentionally OMITS secret_ref and the
// raw endpoint/command — never expose secrets or connection internals here.
// AllowedTools has no omitempty: nil marshals to JSON null, meaning "all tools permitted".
type serverDTO struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Transport       string   `json:"transport"`
	Version         string   `json:"version"`
	ProtocolVersion string   `json:"protocolVersion"`
	Status          string   `json:"status"`
	AllowedTools    []string `json:"allowedTools"`
}

func toServerDTO(m store.MCPServer) serverDTO {
	return serverDTO{
		ID: m.ID, Name: m.Name, Description: m.Description, Transport: m.Transport,
		Version: m.Version, ProtocolVersion: m.ProtocolVersion, Status: m.Status,
	}
}

// handleCatalog lists the MCP servers the caller is entitled to (RBAC-filtered),
// and records an audit event for the access.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	ents, err := s.store.ListEntitlementsByRoles(r.Context(), rc.TenantID, rc.RoleIDs)
	if err != nil {
		fail(w, err)
		return
	}
	visible := rbac.VisibleServerIDs(ents)
	servers, err := s.store.ListMCPServersByTenant(r.Context(), rc.TenantID)
	if err != nil {
		fail(w, err)
		return
	}
	toolsByServer := make(map[string][]string, len(ents))
	seenNilAll := make(map[string]bool, len(ents))
	for _, e := range ents {
		if e.AllowedTools == nil {
			seenNilAll[e.MCPServerID] = true
			continue
		}
		toolsByServer[e.MCPServerID] = append(toolsByServer[e.MCPServerID], e.AllowedTools...)
	}
	out := make([]serverDTO, 0, len(visible))
	// A nil (all-tools) entitlement wins over any restricted ones for the same server.
	for _, sv := range servers {
		// Only "active" servers are live: hide non-active (draft/disabled/…) entries
		// from the catalog even when the caller is entitled to them.
		if sv.Status != "active" {
			continue
		}
		if _, ok := visible[sv.ID]; ok {
			d := toServerDTO(sv)
			if !seenNilAll[sv.ID] {
				d.AllowedTools = toolsByServer[sv.ID]
			}
			out = append(out, d)
		}
	}

	// Best-effort access-log only: a dropped audit write must not fail a read that
	// exposes no secrets (but the drop is logged at Warn, never silent). Mutating
	// actions and deny decisions (P1-d-2b admin CRUD) MUST instead fail the
	// request if their audit write fails — do not copy this best-effort pattern
	// there.
	s.logBestEffortAudit(r.Context(), store.AuditEvent{
		TenantID: rc.TenantID, Actor: p.Subject, Action: "catalog.list",
		Decision: "allow", Metadata: map[string]any{"count": len(out)},
	})

	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}
