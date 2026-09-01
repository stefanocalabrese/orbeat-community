// Package authz resolves a validated token Principal to its database context
// and enforces role-based access on HTTP routes.
package authz

import (
	"context"
	"errors"
	"fmt"
	"time"

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
// upsert (INSERT ... ON CONFLICT). The tenant row is written only when it is
// absent (GetOrCreateTenantByName, internal/store/tenant.go). The users row
// has THREE write reasons, not two: the row is absent, the IdP-sourced fields
// (email, display name) drifted, or last_seen_at has gone stale by more than
// internal/store's unexported lastSeenWriteThreshold, currently one hour,
// which is what keeps a Community seat alive (UpsertUser,
// internal/store/user.go). So audit B4's property is bounded rather than
// absolute: this runs on EVERY authenticated request, and the steady-state
// case, same tenant, same principal, unchanged email and display name, costs
// a single SELECT and no write at all for up to an hour at a time, then one
// UPDATE that carries the refreshed last_seen_at. The fallback upsert keeps
// concurrent first-resolve of the same subject race-safe. Role
// reconciliation (GetRolesByNames) is a plain SELECT with no write path at
// all, so it needed no equivalent change. Any store error aborts with a
// wrapped error rather than returning a partial context.
//
// checkDeactivated runs FIRST, before checkSeatCap and before UpsertUser
// (SCIM deprovisioning, spec docs/specs/2026-08-25-orbeat-scim-design.md
// §2, migration 00021): a person a SCIM caller (or a future admin action)
// deactivated still holds a valid Keycloak token, and without this check
// Resolve would keep upserting and authorizing them forever, making
// deactivation decoration.
//
// The load-bearing part is that it precedes UpsertUser, NOT that it precedes
// checkSeatCap. Refusing before UpsertUser is what stops a deactivated
// person's requests from bumping last_seen_at and so holding a Community
// active-seat slot they no longer legitimately occupy. Swap the two checks
// and that survives untouched, because checkDeactivated still aborts Resolve
// before the upsert. Seat admission is unaffected by the relative order
// either way: checkSeatCap's own existence check (GetUserBySubject) finds a
// deactivated user's row and returns nil, since it only ever gates a
// BRAND-NEW subject.
//
// What the relative order does buy is the "fail fast" an earlier version of
// this comment disclaimed, and only that: in a build with a seat cap,
// refusing here spares the second GetUserBySubject that checkSeatCap would
// otherwise already have spent finding the same row. In this repo's own
// Enterprise build it saves nothing, since checkSeatCap returns on
// seatLimit <= 0 before reading anything.
//
// checkSeatCap runs between the deactivation check and the user upsert
// (spec §3.2, §4): it is what can turn UpsertUser's about-to-happen INSERT
// into a rejected SeatLimitError for a brand-new subject, while never
// rejecting a subject this tenant already knows. See checkSeatCap's own doc
// comment (seatcap.go) for why that ordering is load-bearing. This is also
// the code path cmd/gateway's session build runs through
// (internal/gateway/server.go buildSession calls Resolve directly, not
// through Middleware below), so a capped seat there surfaces as whatever
// buildSession's caller does with a non-nil error: currently a 503 with
// Retry-After, not a response this package controls.
func (r *Resolver) Resolve(ctx context.Context, p auth.Principal) (ResolvedContext, error) {
	tn, err := r.store.GetOrCreateTenantByName(ctx, r.tenantName)
	if err != nil {
		return ResolvedContext{}, fmt.Errorf("authz: resolve tenant: %w", err)
	}
	if err := r.checkDeactivated(ctx, tn.ID, p.Subject); err != nil {
		return ResolvedContext{}, err
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

// ResolveTenant resolves this Resolver's single tenant by name
// (GetOrCreateTenantByName) -- the same call Resolve performs first, above --
// WITHOUT any of Resolve's human-only side effects: no checkSeatCap, no
// UpsertUser, no role reconciliation from p.Roles. It exists for an identity
// source that is not a human token: a virtual key
// (internal/gateway/virtualkey.ee.go, docs/specs/2026-08-25-orbeat-virtual-
// keys-design.md §7) needs the tenant ID to look up its own row by client_id
// before it can even know whether it IS a virtual key, but must never
// consume a Community seat or manufacture a user row for a robot in the
// process of finding out.
func (r *Resolver) ResolveTenant(ctx context.Context) (store.Tenant, error) {
	return r.store.GetOrCreateTenantByName(ctx, r.tenantName)
}

// DeactivatedUserError is returned by Resolve when the principal's existing
// users row has deactivated_at set (SCIM `active: false`, migration 00021).
// Exported, mirroring SeatLimitError (seatcap.go): a caller distinguishes it
// from a genuine failure via errors.As rather than string-matching, so it
// can be surfaced as a 403 ("this identity was deprovisioned") rather than
// SeatLimitError's 402 or a generic error's 500. Middleware (resolved.go)
// does exactly that today, and the SCIM routes this error was built for
// shipped with it: docs/plans/orbeat-scim-2026-08-25.md Tasks 3 and 5 are
// both done (internal/api/scim_users.ee.go, registered admin-only in
// routes_enterprise.ee.go).
//
// Middleware remains the ONLY mapper. cmd/gateway's session build calls
// Resolve directly (internal/gateway/server.go buildSession) and turns any
// non-nil error into a 503 with Retry-After, so an MCP client belonging to a
// deprovisioned identity is refused without being told that is why.
type DeactivatedUserError struct {
	Subject       string
	DeactivatedAt time.Time
}

func (e DeactivatedUserError) Error() string {
	return fmt.Sprintf("subject %q was deactivated at %s", e.Subject, e.DeactivatedAt.Format(time.RFC3339))
}

// checkDeactivated refuses a principal whose users row (if any) has
// deactivated_at set. A subject Resolve has never seen (store.ErrNotFound)
// cannot have been deactivated, so it is admitted here and left to
// UpsertUser to create moments later -- this is not a duplicate of
// checkSeatCap's own existence lookup, since the two run for different
// reasons (see Resolve's doc comment) and neither can be skipped by relying
// on the other having run.
func (r *Resolver) checkDeactivated(ctx context.Context, tenantID, subject string) error {
	u, err := r.store.GetUserBySubject(ctx, tenantID, subject)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("authz: check deactivated: %w", err)
	}
	if u.DeactivatedAt != nil {
		return DeactivatedUserError{Subject: subject, DeactivatedAt: *u.DeactivatedAt}
	}
	return nil
}
