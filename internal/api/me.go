package api

import (
	"net/http"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
)

// meFeaturesDTO is GET /v1/me's "features" object: the edition-dependent
// capabilities the admin console needs so it can stop rendering a control the
// server will 404 on (open-points.md's pinning row, point 6). Read by the
// SAME New(...)-time value every other edition switch on this Server uses
// (Server.pinning, set from pinningSupported() in pinning.ee.go /
// pinning.community.go) — this handler introduces no second opinion about
// what edition is running.
type meFeaturesDTO struct {
	// Pinning mirrors GET /v1/sync/config's own "pinning" key
	// (handleSyncConfig, sync.go): false in every Community build, because
	// PUT /v1/admin/artifacts/{id}/min-revision is registered only by
	// registerEnterpriseRoutes (routes_enterprise.ee.go /
	// routes_enterprise.community.go). Before this field existed the portal
	// had no way to know that, so its "Require this or newer" / floor-clear
	// controls rendered on a Community server, were clickable, and 404'd with
	// a generic error naming no edition at all.
	Pinning bool `json:"pinning"`

	// VirtualKeys is Pinning's sibling for the virtual-keys slice (docs/specs/
	// 2026-08-25-orbeat-virtual-keys-design.md sec 11): false in every
	// Community build, because POST/GET/DELETE /v1/admin/virtual-keys are
	// registered only by registerEnterpriseRoutes. Unlike Pinning, which
	// gates a handful of controls INSIDE an existing page, this gates the
	// existence of an entire admin page (VirtualKeysPage.tsx) -- see that
	// component's own comment for why it renders nothing rather than a
	// disabled shell while this is unknown or false.
	VirtualKeys bool `json:"virtualKeys"`
}

// meResponseDTO is the wire shape of GET /v1/me.
type meResponseDTO struct {
	Subject  string        `json:"subject"`
	Email    string        `json:"email"`
	Roles    []string      `json:"roles"`
	Features meFeaturesDTO `json:"features"`
}

// handleMe returns the caller's token-derived identity plus, as of this
// slice, the edition-capability block above.
//
// WHY THIS RIDES /v1/me AND NOT ONE OF THE TWO OBVIOUS ALTERNATIVES:
//
//   - GET /v1/sync/config already carries "pinning", but that endpoint is
//     orbeat-sync's OWN bootstrap contract (gateway URL, whether deployment
//     reports are accepted). Gating the ADMIN CONSOLE on a document written
//     for a different client with a different lifecycle couples the browser
//     to something that isn't about it.
//   - A new GET /v1/config would be semantically cleanest in isolation, but it
//     would put the SAME boolean on two endpoints that then have to agree
//     forever — a second source of truth on the wire even though the
//     underlying Go value (Server.pinning) is already shared. One value, one
//     place it is read from over HTTP.
//
// /v1/me is already "what can I do here" for a signed-in caller, so this is
// one more field on an existing contract rather than a new one.
//
// Reading s.pinning here is an in-memory field set once by New, NOT a DB
// call — this handler still does no DB resolve at all, the property its own
// registration comment (api.go, "GET /v1/me sits outside both closures
// above") requires and this change must not disturb.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	writeJSON(w, http.StatusOK, meResponseDTO{
		Subject: p.Subject, Email: p.Email, Roles: p.Roles,
		Features: meFeaturesDTO{Pinning: s.pinning, VirtualKeys: s.virtualKeys},
	})
}
