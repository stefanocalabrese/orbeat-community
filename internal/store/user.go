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
	var lastSeenAt time.Time
	err := s.db.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, subject, COALESCE(email,''), COALESCE(display_name,''), last_seen_at
		FROM users WHERE tenant_id = $1 AND subject = $2`,
		u.TenantID, u.Subject,
	).Scan(&existing.ID, &existing.TenantID, &existing.Subject, &existing.Email, &existing.DisplayName, &lastSeenAt)
	switch {
	case err == nil && existing.Email == u.Email && existing.DisplayName == u.DisplayName &&
		time.Since(lastSeenAt) < lastSeenWriteThreshold:
		return existing, nil
	case err == nil:
		// Row exists but either email/display_name drifted from the IdP, or
		// last_seen_at has gone stale; fall through to the write below.
	case errors.Is(err, pgx.ErrNoRows):
		// No row yet; fall through to the write below (first sight of subject).
	default:
		return User{}, fmt.Errorf("upsert user: select: %w", err)
	}

	err = s.db.QueryRow(ctx, `
		INSERT INTO users (tenant_id, subject, email, display_name)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (tenant_id, subject)
		DO UPDATE SET email = EXCLUDED.email, display_name = EXCLUDED.display_name, last_seen_at = now()
		RETURNING id::text`,
		u.TenantID, u.Subject, u.Email, u.DisplayName,
	).Scan(&u.ID)
	if err != nil {
		return User{}, fmt.Errorf("upsert user: write: %w", err)
	}
	return u, nil
}

// GetUserBySubject fetches a user by tenant and IdP subject.
func (s *Store) GetUserBySubject(ctx context.Context, tenantID, subject string) (User, error) {
	var u User
	err := s.db.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, subject, COALESCE(email,''), COALESCE(display_name,'')
		FROM users WHERE tenant_id = $1 AND subject = $2`,
		tenantID, subject,
	).Scan(&u.ID, &u.TenantID, &u.Subject, &u.Email, &u.DisplayName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("get user by subject: %w", err)
	}
	return u, nil
}

// CountActiveUsers counts users in tenantID whose last_seen_at is after
// since. Backs the Community seat cap (Task 4 of this plan, not built here):
// a seat is a user who authenticated within the active window, so the count
// self-heals as users go idle without DeleteUser ever needing to be called
// for that alone (spec sec 3.2 vs 3.3).
func (s *Store) CountActiveUsers(ctx context.Context, tenantID string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE tenant_id = $1 AND last_seen_at > $2`,
		tenantID, since,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active users: %w", err)
	}
	return n, nil
}

// DeletedUser reports the identity DeleteUser destroyed.
//
// Unlike DeleteRole's RevokedGrants, this carries no cascade counts: nothing
// in the schema holds a foreign key to users.id. RBAC roles are reconciled
// from token claims on every request (authz.Resolver.Resolve calls
// GetRolesByNames by name, not by a stored user-role table), and
// audit_event.actor is a free-text subject string, not an FK to this table.
// A user row is a leaf: deleting it destroys exactly that one row and
// nothing else. What is worth reporting back is therefore not a blast
// radius but WHICH identity was removed, so an admin console and the audit
// trail can both name it (spec sec 3.3: "its audit metadata should name what
// it destroyed").
type DeletedUser struct {
	Subject     string
	Email       string
	DisplayName string
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
func (s *Store) DeleteUser(ctx context.Context, tenantID, id string) (DeletedUser, error) {
	var d DeletedUser
	err := s.db.QueryRow(ctx, `
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
