package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// User is a projection of an IdP identity, keyed by (tenant, subject).
type User struct {
	ID          string
	TenantID    string
	Subject     string
	Email       string
	DisplayName string
	// DeactivatedAt is nil for an active user. Non-nil means a SCIM
	// deprovision (or a future admin action) set it, and
	// authz.Resolver.Resolve refuses the request for as long as it stays
	// set -- see migration 00021's comment for why this is not a bool named
	// "active" and mirrors virtual_key.RevokedAt's shape.
	DeactivatedAt *time.Time
}

// lastSeenWriteThreshold bounds how often UpsertUser will write last_seen_at
// for an otherwise-unchanged identity. The Community seat cap (docs/specs/
// 2026-08-19-orbeat-community-caps-design.md sec 3.2) treats a user as an
// active seat for 7 days after their last authenticated request, so a write
// landing anywhere within an hour of the real event is indistinguishable at
// that scale (1 hour is under 1% of the 7-day window). What this constant
// actually buys is bounding write volume on the hottest path in the product
// -- at most one UPDATE per user per hour, no matter how many requests that
// user makes in between -- not precision the seat cap has no use for.
const lastSeenWriteThreshold = time.Hour

// UpsertUser inserts or updates a user by (tenant_id, subject) and returns it.
//
// SELECT-first (audit B4): this runs on every authenticated request via
// authz.Resolver.Resolve. The pre-fix ON-CONFLICT-DO-UPDATE rewrote the user
// row on every request regardless of whether email/display_name had actually
// changed. Steady state (the overwhelming common case, the same principal
// making repeated calls) now costs a single SELECT: the write path only runs
// when the row is absent (first sight of this subject), the IdP-sourced
// fields genuinely changed, or last_seen_at has gone stale (see
// lastSeenWriteThreshold below), so an unchanged identity seen again within
// the threshold produces no WAL record, no new tuple version, and no lock
// contention on the row.
//
// Activity tracking (last_seen_at) rides the SAME write path rather than a
// separate statement: whenever the write below runs, for any of the three
// reasons above, it sets last_seen_at = now(). A second write path that
// touched only last_seen_at would reopen exactly the per-request-write defect
// this function exists to avoid, just under a new name.
func (s *Store) UpsertUser(ctx context.Context, u User) (User, error) {
	var existing User
	// last_seen_at is nullable (migration 00035): a SCIM-provisioned row
	// that has never authenticated carries NULL here, not a sentinel value.
	// A nil lastSeenAt therefore falls through to the write branch below the
	// same way a stale one does -- this person's first real authentication
	// must always be recorded, never treated as "recently seen".
	var lastSeenAt *time.Time
	err := s.db.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, subject, COALESCE(email,''), COALESCE(display_name,''), last_seen_at, deactivated_at
		FROM users WHERE tenant_id = $1 AND subject = $2`,
		u.TenantID, u.Subject,
	).Scan(&existing.ID, &existing.TenantID, &existing.Subject, &existing.Email, &existing.DisplayName, &lastSeenAt, &existing.DeactivatedAt)
	switch {
	case err == nil && existing.Email == u.Email && existing.DisplayName == u.DisplayName &&
		lastSeenAt != nil && time.Since(*lastSeenAt) < lastSeenWriteThreshold:
		return existing, nil
	case err == nil:
		// Row exists but either email/display_name drifted from the IdP,
		// last_seen_at has gone stale, or last_seen_at is NULL (a
		// provisioned-but-never-authenticated row seeing its first real
		// login); fall through to the write below.
	case errors.Is(err, pgx.ErrNoRows):
		// No row yet; fall through to the write below (first sight of subject).
	default:
		return User{}, fmt.Errorf("upsert user: select: %w", err)
	}

	// The DO UPDATE SET list deliberately omits deactivated_at: a SCIM
	// deprovision must survive every later request from the same subject,
	// and this statement runs on EVERY authenticated request via
	// authz.Resolver.Resolve. Naming it here (even as
	// "deactivated_at = deactivated_at") would invite a future edit to
	// "helpfully" reset it; omitting it entirely means there is nothing on
	// this line for that edit to reach. RETURNING reads back the column so
	// the returned User reflects the row's real state either way (unchanged
	// on the UPDATE branch, NULL on the INSERT branch, since a brand-new
	// user has never been deactivated).
	err = s.db.QueryRow(ctx, `
		INSERT INTO users (tenant_id, subject, email, display_name)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant_id, subject)
		DO UPDATE SET email = EXCLUDED.email, display_name = EXCLUDED.display_name, last_seen_at = now()
		RETURNING id::text, deactivated_at`,
		u.TenantID, u.Subject, u.Email, u.DisplayName,
	).Scan(&u.ID, &u.DeactivatedAt)
	if err != nil {
		return User{}, fmt.Errorf("upsert user: write: %w", err)
	}
	return u, nil
}

// GetUserBySubject fetches a user by tenant and IdP subject.
func (s *Store) GetUserBySubject(ctx context.Context, tenantID, subject string) (User, error) {
	var u User
	err := s.db.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, subject, COALESCE(email,''), COALESCE(display_name,''), deactivated_at
		FROM users WHERE tenant_id = $1 AND subject = $2`,
		tenantID, subject,
	).Scan(&u.ID, &u.TenantID, &u.Subject, &u.Email, &u.DisplayName, &u.DeactivatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("get user by subject: %w", err)
	}
	return u, nil
}

// GetUserByID fetches a user by tenant and row id -- the same id
// DeleteUser/DeactivateUser/ReactivateUser already take, and the id SCIM's
// own `/Users/{id}` path uses (docs/specs/2026-08-25-orbeat-scim-design.md
// sec 4). Malformed id (SQLSTATE 22P02) maps to ErrNotFound via
// idCastNotFound, mirroring GetMCPServer/GetArtifact.
//
// Added for Task 3 of docs/plans/orbeat-scim-2026-08-25.md: that task's own
// file list named only internal/api/scim_users.ee.go and
// routes_enterprise.ee.go, but GET/PATCH /scim/v2/Users/{id} cannot fetch
// the row they act on without an id-keyed lookup, and no such lookup existed
// (GetUserBySubject is keyed by subject, not id). Lives in this shared
// (non-.ee) file rather than internal/scim or an .ee.go file because it is
// an ordinary store accessor with no SCIM-specific shape, matching how
// DeactivateUser/ReactivateUser (Task 1) already sit here despite existing
// only for SCIM's benefit today.
func (s *Store) GetUserByID(ctx context.Context, tenantID, id string) (User, error) {
	var u User
	err := s.db.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, subject, COALESCE(email,''), COALESCE(display_name,''), deactivated_at
		FROM users WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	).Scan(&u.ID, &u.TenantID, &u.Subject, &u.Email, &u.DisplayName, &u.DeactivatedAt)
	if err != nil {
		if idCastNotFound(err) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("get user by id: %w", err)
	}
	return u, nil
}

// ListUsersByTenant returns every user row for tenantID, subject-ordered.
// Backs SCIM's unfiltered `GET /scim/v2/Users` (spec sec 4) -- added for the
// same reason as GetUserByID above (Task 3's file list did not anticipate
// it, but the endpoint table it specifies cannot be built without it).
//
// Deliberately unpaginated, unlike ListMCPServersPage/ListRolesPage and
// their siblings: SCIM's own out-of-scope list (spec sec 5) already excludes
// bulk operations and complex filters, no gate in Task 3 asks for
// startIndex/count support, and this repo's SCIM directory is one tenant's
// admin-managed user list, not a resource with the row counts that made
// keyset pagination load-bearing elsewhere (fable-audit §7 #8). Revisit if
// an operator's user count ever makes that untrue.
func (s *Store) ListUsersByTenant(ctx context.Context, tenantID string) ([]User, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id::text, tenant_id::text, subject, COALESCE(email,''), COALESCE(display_name,''), deactivated_at
		FROM users WHERE tenant_id = $1 ORDER BY subject`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list users by tenant: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.TenantID, &u.Subject, &u.Email, &u.DisplayName, &u.DeactivatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeactivateUser sets deactivated_at to now() (SCIM `active: false`,
// migration 00021), tenant-scoped. Idempotent: deactivating an
// already-deactivated user just re-stamps the timestamp rather than
// erroring, mirroring RevokeVirtualKey's re-revoke behavior. Returns
// ErrNotFound for an unknown id, a cross-tenant id, or a malformed one
// (SQLSTATE 22P02 via idCastNotFound), mirroring DeleteUser.
func (s *Store) DeactivateUser(ctx context.Context, tenantID, id string) error {
	ct, err := s.db.Exec(ctx,
		`UPDATE users SET deactivated_at = now() WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	)
	if err != nil {
		if idCastNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("deactivate user: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ReactivateUser clears deactivated_at (SCIM `active: true`), tenant-scoped.
// Idempotent, and returns ErrNotFound the same way DeactivateUser does.
func (s *Store) ReactivateUser(ctx context.Context, tenantID, id string) error {
	ct, err := s.db.Exec(ctx,
		`UPDATE users SET deactivated_at = NULL WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	)
	if err != nil {
		if idCastNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("reactivate user: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountActiveUsers counts users in tenantID whose last_seen_at is after
// since AND who are not deactivated. Backs the Community seat cap (Task 4 of
// this plan, not built here): a seat is a user who authenticated within the
// active window, so the count self-heals as users go idle without
// DeleteUser ever needing to be called for that alone (spec sec 3.2 vs 3.3).
//
// The deactivated_at IS NULL predicate closes one third of audit B9: without
// it, a SCIM-deactivated user (migration 00021, DeactivateUser) kept
// counting as an active seat for the REST of the 7-day activeSeatWindow
// (authz/seatcap.go) after being deprovisioned — deactivation revoked their
// ACCESS immediately (authz.Resolver.Resolve's checkDeactivated refuses
// them) but did not free the SEAT they were occupying, so an operator who
// deactivated a departing contractor still saw the tenant at its cap until
// the window happened to elapse on its own. Excluding a deactivated row here
// makes the seat available again the instant deactivation is recorded,
// mirroring how it already makes ACCESS unavailable the instant it is
// recorded.
func (s *Store) CountActiveUsers(ctx context.Context, tenantID string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE tenant_id = $1 AND last_seen_at > $2 AND deactivated_at IS NULL`,
		tenantID, since,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active users: %w", err)
	}
	return n, nil
}

// UpsertProvisionedUser inserts or updates a user by (tenant_id, subject) the
// way an identity directory push does (SCIM create/patch), and is
// UpsertUser's SCIM-safe sibling (audit B9, sub-defects 2 and 3).
//
// UpsertUser conflates two different events under one write path: "a human
// just authenticated" (authz.Resolver.Resolve, where last_seen_at = now() IS
// the fact being recorded) and "an IdP just pushed a directory change"
// (SCIM, never a login). Using UpsertUser for both let a SCIM-provisioned
// user consume a Community seat the instant its row landed — before it ever
// authenticated once — and let a later SCIM displayName PATCH silently
// refresh that seat's 7-day clock, both because UpsertUser's write path sets
// last_seen_at = now() unconditionally whenever it writes at all. This
// function never does:
//
//   - INSERT (row does not exist yet): last_seen_at is written as NULL, not
//     now() — the OPPOSITE of the column's own DEFAULT, so this statement
//     must name it explicitly rather than omit it (omitting it here would
//     silently fall back to the column default and reopen sub-defect 2).
//     NULL is the schema-correct representation of "never authenticated"
//     (migration 00035; before it, a documented Unix-epoch sentinel stood
//     in for this exact value, since a migration was out of that lane's
//     scope). store.CountActiveUsers' `last_seen_at > since` predicate
//     excludes a NULL row automatically (SQL's three-valued logic: NULL
//     compared to anything is NULL, and WHERE treats NULL as false), so it
//     never counts a freshly provisioned, never-logged-in row until the
//     person's first real authz.Resolver.Resolve.
//   - UPDATE (row exists): the SET list omits last_seen_at entirely — same
//     shape as UpsertUser's own deactivated_at omission, and for the
//     identical reason (that function's own doc comment): naming it, even
//     as "last_seen_at = last_seen_at", invites a future edit to
//     "helpfully" bump it. An admin or IdP editing a profile field can
//     therefore never look like the person logging back in.
//     deactivated_at is likewise never named here, mirroring UpsertUser: a
//     profile PATCH must never silently reactivate a deprovisioned user.
func (s *Store) UpsertProvisionedUser(ctx context.Context, u User) (User, error) {
	err := s.db.QueryRow(ctx, `
		INSERT INTO users (tenant_id, subject, email, display_name, last_seen_at)
		VALUES ($1,$2,$3,$4,NULL)
		ON CONFLICT (tenant_id, subject)
		DO UPDATE SET email = EXCLUDED.email, display_name = EXCLUDED.display_name
		RETURNING id::text, deactivated_at`,
		u.TenantID, u.Subject, u.Email, u.DisplayName,
	).Scan(&u.ID, &u.DeactivatedAt)
	if err != nil {
		return User{}, fmt.Errorf("upsert provisioned user: %w", err)
	}
	return u, nil
}

// DeletedUser reports the identity DeleteUser destroyed.
//
// A user row is NO LONGER a leaf, and this comment said it was until
// migration 00017 landed. artifact_deployment.user_id REFERENCES users(id) ON
// DELETE CASCADE is the FIRST of two foreign keys in this schema pointing at
// users.id, so deleting a user now also destroys every row recording which
// artifact revision each of that person's machines had. The second is
// virtual_key.created_by (00020), which is ON DELETE SET NULL rather than
// CASCADE: the key survives the person, because it is owned by a role. This
// comment enumerated the cascade set without it until audit finding C7, and
// audit B33 made that omission doubly misleading on the same day by giving
// DeleteUser an explicit revoke of those keys.
//
// That cascade is the design rather than a side effect. DELETE
// /v1/admin/users/{id} is the erasure path for the deployment records about
// one named individual (docs/specs/2026-08-22-orbeat-artifact-deployment-
// registry-design.md sec 8.3), and leaving those rows behind would be a
// per-person data remnant whose owner no longer exists.
//
// Access, though, still cascades nowhere, and that is the part operators keep
// needing told: RBAC roles are reconciled from token claims on every request
// (authz.Resolver.Resolve calls GetRolesByNames by name, not by a stored
// user-role table), and audit_event.actor is a free-text subject string, not
// an FK to this table. Deleting a user revokes nothing and bans nobody.
//
// Unlike DeleteRole's RevokedGrants, this struct still carries no cascade
// count. What is worth reporting back is WHICH identity was removed, so an
// admin console and the audit trail can both name it (spec sec 3.3: "its
// audit metadata should name what it destroyed"); whether it should also
// report how many deployment rows went with it is an open question for the
// registry slice, and the argument against is that the count is a privacy
// fact about the deleted person's machines on a response that goes to an
// admin console.
type DeletedUser struct {
	Subject              string
	Email                string
	DisplayName          string
	RevokedVirtualKeyIDs []string // B33: virtual key IDs revoked as part of this delete
}

// DeleteUser removes a user row scoped to its tenant and reports the
// identity it destroyed.
//
// A single DELETE ... RETURNING, not DeleteRole's lock-then-read-then-delete
// sequence: DeleteRole's SELECT ... FOR UPDATE exists to serialize against a
// concurrent read of OTHER tables (entitlement, artifact_entitlement) taken
// between the lock and the final DELETE, because RoleExistsInTenant holds a
// FOR SHARE lock on the role row open across an INSERT elsewhere, and reading
// those other tables' counts before acquiring that lock races against it
// (see DeleteRole's doc comment and TestDeleteRoleLocksAgainstConcurrentGrantInsert).
// A user row has no second table to read before the delete: everything this
// function reports (subject, email, display_name) lives on the row being
// deleted itself, so DELETE ... RETURNING captures it atomically in the same
// statement that removes it. There is no separate read for a concurrent
// writer to invalidate, so there is nothing for a lock to protect.
//
// Returns ErrNotFound for an unknown id, a cross-tenant id, and a malformed
// one (SQLSTATE 22P02 via idCastNotFound), mirroring DeleteRole/DeleteEntitlement.
//
// B33: before deleting the user, this function revokes every virtual key whose
// created_by points at this row. The revoked key IDs are returned in
// DeletedUser.RevokedVirtualKeyIDs so the audit trail can name them. The ON
// DELETE SET NULL on virtual_key.created_by still fires (the keys survive as
// role-owned rows), but they are marked revoked so the gateway rejects them.
func (s *Store) DeleteUser(ctx context.Context, tenantID, id string) (DeletedUser, error) {
	var d DeletedUser

	// Check the user exists first: a malformed id would 22P02 in the revoke
	// SQL below before we ever reach the DELETE, and we must return ErrNotFound
	// for that case (TestDeleteUserNotFound/malformed_id). A 22P02 on the
	// existence check means no user row can match, so treat it as not found.
	var exists int
	if err := s.db.QueryRow(ctx, `SELECT count(*)::int FROM users WHERE tenant_id=$1 AND id=$2`,
		tenantID, id).Scan(&exists); err != nil {
		if idCastNotFound(err) {
			return DeletedUser{}, ErrNotFound
		}
		return DeletedUser{}, fmt.Errorf("check user exists before delete: %w", err)
	}
	if exists == 0 {
		return DeletedUser{}, ErrNotFound
	}

	// B33: revoke all virtual keys created by this user before the delete.
	// A single UPDATE with a subquery is atomic and avoids holding locks on
	// individual key rows longer than necessary. The bump_row_version trigger
	// fires on this UPDATE (revoked_at changes), but that is correct: the key
	// row_version must advance when its state changes, even if the change is
	// "revoked because creator was deleted." The B30 test (which checks that
	// ON DELETE SET NULL does NOT bump row_version) is unaffected because it
	// tests the trigger on SET NULL, not on UPDATE.
	const revokeSQL = `
		WITH matched AS (
			SELECT id FROM virtual_key WHERE tenant_id=$1 AND created_by=$2
		)
		UPDATE virtual_key SET revoked_at = now()
		WHERE tenant_id=$1 AND id IN (SELECT id FROM matched)
		RETURNING id`
	rows, err := s.db.Query(ctx, revokeSQL, tenantID, id)
	if err != nil {
		return DeletedUser{}, fmt.Errorf("revoke virtual keys before delete user: %w", err)
	}
	d.RevokedVirtualKeyIDs = make([]string, 0)
	for rows.Next() {
		var kid string
		if err := rows.Scan(&kid); err != nil {
			rows.Close()
			return DeletedUser{}, fmt.Errorf("scan revoked virtual key id: %w", err)
		}
		d.RevokedVirtualKeyIDs = append(d.RevokedVirtualKeyIDs, kid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DeletedUser{}, fmt.Errorf("iterate revoked virtual keys: %w", err)
	}
	rows.Close()

	err = s.db.QueryRow(ctx, `
		DELETE FROM users WHERE tenant_id = $1 AND id = $2
		RETURNING subject, COALESCE(email,''), COALESCE(display_name,'')`,
		tenantID, id,
	).Scan(&d.Subject, &d.Email, &d.DisplayName)
	if err != nil {
		if idCastNotFound(err) {
			return DeletedUser{}, ErrNotFound
		}
		return DeletedUser{}, fmt.Errorf("delete user: %w", err)
	}
	return d, nil
}
