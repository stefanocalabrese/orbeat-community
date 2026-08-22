package api

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/marketplace"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// syncArtifactDTO is one entitled artifact for the Channel-2 sync client. Content
// is the final, ready-to-write file body (subagent memory frontmatter already
// injected); the client writes it verbatim and derives the path from type+name.
// MemoryScope/MemorySeed are set only for user/project-scope subagents carrying
// a non-empty seed (spec §6) — the sync client uses them to seed the target
// memory file. Seed fields are omitted otherwise — "" and absent are equivalent
// everywhere (empty seed = no seed, spec §4); omitempty just keeps non-seeded
// artifacts' payloads clean.
type syncArtifactDTO struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Content     string `json:"content"`
	MemoryScope string `json:"memoryScope,omitempty"` // set only alongside memorySeed (target-path selection)
	MemorySeed  string `json:"memorySeed,omitempty"`  // governed ORBEAT-SEED block body
}

// handleSyncArtifacts returns the caller's entitled role-visibility artifacts,
// rendered to final file content. RBAC-filtered server-side by the caller's roles;
// org-visibility artifacts are never returned here (they ship via the Channel-1 plugin).
func (s *Server) handleSyncArtifacts(w http.ResponseWriter, r *http.Request) {
	rc, p, ok := s.resolved(w, r)
	if !ok {
		return
	}
	arts, err := s.store.ListEntitledArtifacts(r.Context(), rc.TenantID, rc.RoleIDs)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]syncArtifactDTO, 0, len(arts))
	for _, a := range arts {
		content := marketplace.RenderArtifactContent(marketplace.Artifact{
			Type: a.Type, Name: a.Name, Content: a.Content, MemoryScope: a.MemoryScope,
		})
		dto := syncArtifactDTO{Type: a.Type, Name: a.Name, Content: content}
		// A seed is only deliverable to user/project-scope subagents (spec §6);
		// Go-level gate mirrors the admin validation, fail-closed.
		if a.Type == "subagent" && a.MemorySeed != "" && (a.MemoryScope == "user" || a.MemoryScope == "project") {
			dto.MemoryScope, dto.MemorySeed = a.MemoryScope, a.MemorySeed
		}
		out = append(out, dto)
	}
	// Best-effort access log (mirrors catalog.list; exposes no secrets). A dropped
	// audit write must not fail a read, but the drop is logged at Warn.
	s.logBestEffortAudit(r.Context(), store.AuditEvent{
		TenantID: rc.TenantID, Actor: p.Subject, Action: "sync.list",
		Decision: "allow", Metadata: map[string]any{"count": len(out)},
	})
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": out})
}

// handleSyncConfig advertises client bootstrap config — currently just the
// gateway resource URL, so orbeat-sync can write it into each tool's MCP config
// without a second URL knob. Authenticated (normal-user); the URL is not secret,
// but gating keeps the surface consistent.
func (s *Server) handleSyncConfig(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.resolved(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gateway_url": s.gatewayURL})
}
