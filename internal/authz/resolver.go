// Package authz resolves a validated token Principal to its database context
// and enforces role-based access on HTTP routes.
package authz

import (
	"context"
	"fmt"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// Resolver maps a Principal to its DB tenant/user and reconciled role IDs.
type Resolver struct {
	store      *store.Store
	tenantName string

	// seatLimit is the Community edition's active-seat cap (0 = unlimited,
	// NewResolver's default via communitySeatLimit, the extension-point
	// pair seatlimit.ee.go/seatlimit.community.go). Unexported: nothing
	// outside a test in this package ever needs to change it, since the
	// edition IS the build, not a runtime choice.
	seatLimit int

	// contactEmail is the address a seat-cap 402 response points an admin
	// at. Defaults to DefaultContactEmail; cmd/api wires ORBEAT_CONTACT_EMAIL
	// to override it via SetContactEmail.
	contactEmail string
}

// NewResolver constructs a Resolver bound to a single tenant (by name).
func NewResolver(s *store.Store, tenantName string) *Resolver {
	return &Resolver{
		store: s, tenantName: tenantName,
		seatLimit: communitySeatLimit(), contactEmail: DefaultContactEmail,
	}
}

// SetContactEmail overrides the seat-cap 402 response's contact address
// (default DefaultContactEmail, set by NewResolver). Empty is ignored,
// mirroring internal/api's Set* nil/empty-ignore contract, so a caller can
// never accidentally blank the default.
func (r *Resolver) SetContactEmail(email string) {
	if email != "" {
		r.contactEmail = email
	}
}

// ResolvedContext is the database-side identity for a request.
type ResolvedContext struct {
	TenantID string
	UserID   string
	RoleIDs  []string // DB role IDs for the principal's token roles (unknown names dropped)
}

// Resolve ensures the tenant and user exist and reconciles token role names to
// DB role IDs. Tenant and user are SELECT-first, falling back to an atomic
// upsert (INSERT ... ON CONFLICT) only when the row is absent or the user's
// IdP-sourced fields changed (audit B4: this runs on EVERY authenticated
// request, so the steady-state case — same tenant, same principal,
// unchanged email/display name — must not rewrite either row). The fallback
// upsert keeps concurrent first-resolve of the same subject race-safe. Role
// reconciliation (GetRolesByNames) is a plain SELECT with no write path at
// all, so it needed no equivalent change. Any store error aborts with a
// wrapped error rather than returning a partial context.
//
// checkSeatCap runs between the tenant resolve and the user upsert (spec
// §3.2, §4): it is what can turn UpsertUser's about-to-happen INSERT into a
// rejected SeatLimitError for a brand-new subject, while never rejecting a
// subject this tenant already knows. See checkSeatCap's own doc comment
// (seatcap.go) for why that ordering is load-bearing. This is also the code
// path cmd/gateway's session build runs through (internal/gateway/server.go
// buildSession calls Resolve directly, not through Middleware below), so a
// capped seat there surfaces as whatever buildSession's caller does with a
// non-nil error: currently a 503 with Retry-After, not a response this
// package controls.
func (r *Resolver) Resolve(ctx context.Context, p auth.Principal) (ResolvedContext, error) {
	tn, err := r.store.GetOrCreateTenantByName(ctx, r.tenantName)
	if err != nil {
		return ResolvedContext{}, fmt.Errorf("authz: resolve tenant: %w", err)
	}
	if err := r.checkSeatCap(ctx, tn.ID, p.Subject); err != nil {
		return ResolvedContext{}, err
	}
	u, err := r.store.UpsertUser(ctx, store.User{TenantID: tn.ID, Subject: p.Subject, Email: p.Email, DisplayName: p.Email})
	if err != nil {
		return ResolvedContext{}, fmt.Errorf("authz: resolve user: %w", err)
	}
	roles, err := r.store.GetRolesByNames(ctx, tn.ID, p.Roles)
	if err != nil {
		return ResolvedContext{}, fmt.Errorf("authz: reconcile roles: %w", err)
	}
	ids := make([]string, 0, len(roles))
	for _, role := range roles {
		ids = append(ids, role.ID)
	}
	return ResolvedContext{TenantID: tn.ID, UserID: u.ID, RoleIDs: ids}, nil
}
