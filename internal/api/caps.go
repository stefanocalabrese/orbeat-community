package api

import (
	"context"
)

// checkServerActiveCap enforces editionLimits.Servers (editionlimits.go)
// before a create or an update that would leave the server active. Spec §4:
// only status="active" counts, so deactivating a server frees a slot
// immediately, and a create or update to "disabled" is never capped:
// max<=0 or status!="active" both short-circuit before any store read.
//
// excludeID is the id of the server being updated ("" for create), so
// updating an ALREADY-active server's other fields, same status, same id,
// never counts itself against its own cap; only OTHER active servers are
// counted.
//
// This is an advisory pre-check, not a transactional guard: it reads outside
// the create/update transaction, so two concurrent requests can both observe
// a count under the cap and both succeed, landing one over. That mirrors
// checkServerSlugCollision (admin_servers.go), which documents the identical
// trade-off for the same reason (a full serializing lock around a rare
// admin-console write is not worth it). No fail-safe backstop exists for
// this cap the way the gateway's slug guard backstops that one, so the
// window is a known, accepted limitation, not a defect.
func (s *Server) checkServerActiveCap(ctx context.Context, tenantID, excludeID, status string) error {
	max := s.limits.Servers
	if max <= 0 || status != "active" {
		return nil
	}
	servers, err := s.store.ListMCPServersByTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	current := 0
	for _, m := range servers {
		if m.Status == "active" && m.ID != excludeID {
			current++
		}
	}
	if current >= max {
		return limitError{Resource: "servers", Max: max, Current: current, Contact: s.contactEmail}
	}
	return nil
}

// checkRoleCap enforces editionLimits.Roles (editionlimits.go) before a role
// create (spec §4). A role has no "inactive" state, so unlike servers, every
// existing row counts and there is no excludeID to worry about. Same
// advisory pre-check trade-off as checkServerActiveCap above.
func (s *Server) checkRoleCap(ctx context.Context, tenantID string) error {
	max := s.limits.Roles
	if max <= 0 {
		return nil
	}
	roles, err := s.store.ListRolesPage(ctx, tenantID, nil, 0, false, "")
	if err != nil {
		return err
	}
	if len(roles) >= max {
		return limitError{Resource: "roles", Max: max, Current: len(roles), Contact: s.contactEmail}
	}
	return nil
}
