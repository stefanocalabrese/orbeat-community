package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUserUpsertAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	u1, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|123", Email: "a@x.io", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	if u1.ID == "" {
		t.Fatal("expected non-empty ID")
	}

	// Upserting the same subject updates, keeps the same ID.
	u2, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|123", Email: "a2@x.io", DisplayName: "Alice A.",
	})
	if err != nil {
		t.Fatalf("UpsertUser(2): %v", err)
	}
	if u2.ID != u1.ID {
		t.Fatalf("expected stable ID on upsert, got %s then %s", u1.ID, u2.ID)
	}
	if u2.Email != "a2@x.io" || u2.DisplayName != "Alice A." {
		t.Fatalf("expected updated fields, got %+v", u2)
	}

	got, err := s.GetUserBySubject(ctx, tn.ID, "kc|123")
	if err != nil {
		t.Fatalf("GetUserBySubject: %v", err)
	}
	if got.ID != u1.ID || got.Email != "a2@x.io" {
		t.Fatalf("GetUserBySubject = %+v", got)
	}
}

// TestUpsertUserSteadyStateNoWrite pins audit B4: once a user row exists, a
// repeat UpsertUser with IDENTICAL email/display_name must not rewrite it
// (the common case — the same principal calling repeatedly with an unchanged
// IdP identity). xmin (bumped by Postgres on every UPDATE, even a no-op one)
// is the change-detector. Pre-fix, this test fails: the unconditional
// ON-CONFLICT-DO-UPDATE rewrites the row on every call regardless of whether
// anything changed. A subsequent genuine email change MUST still propagate
// and MUST still advance xmin — proving the write-avoidance didn't silently
// disable real updates.
func TestUpsertUserSteadyStateNoWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	u1, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|steady", Email: "a@x.io", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	xminBefore := userXmin(t, s, u1.ID)

	u2, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|steady", Email: "a@x.io", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("UpsertUser(steady repeat): %v", err)
	}
	if u2.ID != u1.ID {
		t.Fatalf("expected stable ID, got %s then %s", u1.ID, u2.ID)
	}
	xminAfterSteady := userXmin(t, s, u1.ID)
	if xminAfterSteady != xminBefore {
		t.Fatalf("steady-state UpsertUser (identical fields) rewrote the user row: xmin %s -> %s (expected a SELECT-only fast path, no write)", xminBefore, xminAfterSteady)
	}

	// A genuine email change MUST still write.
	u3, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|steady", Email: "changed@x.io", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("UpsertUser(changed email): %v", err)
	}
	if u3.Email != "changed@x.io" {
		t.Fatalf("expected the email change to propagate, got %+v", u3)
	}
	xminAfterChange := userXmin(t, s, u1.ID)
	if xminAfterChange == xminAfterSteady {
		t.Fatalf("expected an email change to rewrite the user row (xmin unchanged: %s), but write-avoidance must not swallow real updates", xminAfterChange)
	}
}

// userXmin reads the raw Postgres system column xmin for row id, as a
// change-detector: any UPDATE (even one writing identical values) advances it.
func userXmin(t *testing.T, s *Store, id string) string {
	t.Helper()
	var xmin string
	if err := s.db.QueryRow(context.Background(), `SELECT xmin::text FROM users WHERE id = $1`, id).Scan(&xmin); err != nil {
		t.Fatalf("query user xmin: %v", err)
	}
	return xmin
}

// userLastSeenAt reads the raw last_seen_at column for row id.
func userLastSeenAt(t *testing.T, s *Store, id string) time.Time {
	t.Helper()
	var ts time.Time
	if err := s.db.QueryRow(context.Background(), `SELECT last_seen_at FROM users WHERE id = $1`, id).Scan(&ts); err != nil {
		t.Fatalf("query user last_seen_at: %v", err)
	}
	return ts
}

// TestUpsertUserWritesWhenStale is the other half of
// TestUpsertUserSteadyStateNoWrite: that test pins "no write within the
// threshold"; this one pins "a write DOES happen once last_seen_at has gone
// stale," with an otherwise byte-identical principal (same email, same
// display name). Without this test, lastSeenWriteThreshold's write path could
// be dead code: a gate that never fires for staleness would leave every
// existing user's last_seen_at frozen at whatever value UpsertUser (or the
// 00015 backfill) first set it to, which would eventually evict every real
// active user from the Community seat count instead of the abandoned ones.
//
// last_seen_at is backdated directly via raw SQL (bypassing UpsertUser) to
// simulate a row that has genuinely gone stale, since forcing an hour of real
// wall-clock time to pass in a test is not practical.
func TestUpsertUserWritesWhenStale(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	u1, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|stale", Email: "a@x.io", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	xminBefore := userXmin(t, s, u1.ID)

	// Backdate last_seen_at past lastSeenWriteThreshold, as if this row has
	// not been touched by a real request in over a day.
	backdated := time.Now().Add(-24 * time.Hour)
	if _, err := s.db.Exec(ctx, `UPDATE users SET last_seen_at = $1 WHERE id = $2`, backdated, u1.ID); err != nil {
		t.Fatalf("backdate last_seen_at: %v", err)
	}

	u2, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|stale", Email: "a@x.io", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("UpsertUser(stale repeat): %v", err)
	}
	if u2.ID != u1.ID {
		t.Fatalf("expected stable ID, got %s then %s", u1.ID, u2.ID)
	}

	xminAfter := userXmin(t, s, u1.ID)
	if xminAfter == xminBefore {
		t.Fatalf("stale UpsertUser (identical fields, backdated last_seen_at) did not rewrite the user row: xmin unchanged (%s)", xminAfter)
	}
	lastSeen := userLastSeenAt(t, s, u1.ID)
	if time.Since(lastSeen) >= lastSeenWriteThreshold {
		t.Fatalf("expected last_seen_at to advance to ~now on a stale write, got %s (backdated was %s)", lastSeen, backdated)
	}
}

// TestDeactivateUserAndReactivate is the store-level round trip for SCIM
// deprovisioning (migration 00021): DeactivateUser sets deactivated_at,
// GetUserBySubject observes it, ReactivateUser clears it again. Also checks
// idempotency in both directions (a second Deactivate/Reactivate on the same
// state is not rejected), mirroring RevokeVirtualKey's re-revoke contract.
func TestDeactivateUserAndReactivate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	u, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|scim", Email: "scim@x.io", DisplayName: "Scim"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if u.DeactivatedAt != nil {
		t.Fatalf("a freshly upserted user must not be deactivated, got %v", u.DeactivatedAt)
	}

	before := time.Now()
	if err := s.DeactivateUser(ctx, tn.ID, u.ID); err != nil {
		t.Fatalf("DeactivateUser: %v", err)
	}
	got, err := s.GetUserBySubject(ctx, tn.ID, "kc|scim")
	if err != nil {
		t.Fatalf("GetUserBySubject: %v", err)
	}
	if got.DeactivatedAt == nil {
		t.Fatal("expected deactivated_at to be set after DeactivateUser")
	}
	if got.DeactivatedAt.Before(before.Add(-time.Second)) {
		t.Fatalf("deactivated_at = %s, want at/after %s", got.DeactivatedAt, before)
	}

	// Idempotent: deactivating an already-deactivated user re-stamps rather
	// than erroring.
	if err := s.DeactivateUser(ctx, tn.ID, u.ID); err != nil {
		t.Fatalf("second DeactivateUser must not error, got %v", err)
	}

	if err := s.ReactivateUser(ctx, tn.ID, u.ID); err != nil {
		t.Fatalf("ReactivateUser: %v", err)
	}
	got, err = s.GetUserBySubject(ctx, tn.ID, "kc|scim")
	if err != nil {
		t.Fatalf("GetUserBySubject after reactivate: %v", err)
	}
	if got.DeactivatedAt != nil {
		t.Fatalf("expected deactivated_at to be cleared after ReactivateUser, got %v", got.DeactivatedAt)
	}

	// Idempotent the other direction too.
	if err := s.ReactivateUser(ctx, tn.ID, u.ID); err != nil {
		t.Fatalf("second ReactivateUser must not error, got %v", err)
	}
}

// TestDeactivateUserNotFound mirrors TestDeleteUserNotFound's three shapes
// (unknown id, malformed id, cross-tenant id) for DeactivateUser and
// ReactivateUser, and checks the cross-tenant user's state is untouched.
func TestDeactivateUserNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)
	other := mustTenant(t, s)

	foreign, err := s.UpsertUser(ctx, User{TenantID: other.ID, Subject: "kc|foreign-deact", Email: "f@x.io"})
	if err != nil {
		t.Fatalf("upsert foreign user: %v", err)
	}

	cases := []struct {
		name string
		id   string
	}{
		{"unknown uuid", "00000000-0000-0000-0000-000000000000"},
		{"malformed id", "not-a-uuid"},
		{"cross-tenant id", foreign.ID},
	}
	for _, tc := range cases {
		t.Run("deactivate/"+tc.name, func(t *testing.T) {
			if err := s.DeactivateUser(ctx, tn.ID, tc.id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("DeactivateUser(%s) = %v, want ErrNotFound", tc.name, err)
			}
		})
		t.Run("reactivate/"+tc.name, func(t *testing.T) {
			if err := s.ReactivateUser(ctx, tn.ID, tc.id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("ReactivateUser(%s) = %v, want ErrNotFound", tc.name, err)
			}
		})
	}

	got, err := s.GetUserBySubject(ctx, other.ID, "kc|foreign-deact")
	if err != nil {
		t.Fatalf("expected the foreign user to still exist: %v", err)
	}
	if got.DeactivatedAt != nil {
		t.Fatalf("cross-tenant DeactivateUser must not have touched the foreign user, got %v", got.DeactivatedAt)
	}
}

// TestUpsertUserPreservesDeactivation is the store-level red-proof for gate 2
// of docs/plans/orbeat-scim-2026-08-25.md Task 1: UpsertUser runs on EVERY
// authenticated request (authz.Resolver.Resolve), so if its DO UPDATE SET
// ever cleared deactivated_at, a deactivated person's very next request
// would silently revive them. Both write-path triggers that call
// (v1.5, package comment) says can fire the write branch are exercised: a
// changed field (forces the write) and a stale last_seen_at (also forces the
// write) -- the steady-state fast path (SELECT-only, no write at all) can
// never clear the column either, but proves nothing about the DO UPDATE SET
// clause since it never executes.
//
// Verified by hand: adding "deactivated_at = NULL" to UpsertUser's DO UPDATE
// SET makes both assertions below fail (the row comes back reactivated).
func TestUpsertUserPreservesDeactivation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	u, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|preserve", Email: "a@x.io", DisplayName: "Alice"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.DeactivateUser(ctx, tn.ID, u.ID); err != nil {
		t.Fatalf("DeactivateUser: %v", err)
	}

	// Trigger 1: a changed field forces UpsertUser's write path.
	u2, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|preserve", Email: "changed@x.io", DisplayName: "Alice"})
	if err != nil {
		t.Fatalf("UpsertUser(changed email): %v", err)
	}
	if u2.DeactivatedAt == nil {
		t.Fatal("UpsertUser's write path (changed field) cleared deactivated_at; " +
			"a deactivated user's next request must not silently revive them")
	}

	// Trigger 2: a stale last_seen_at forces the write path even with
	// identical fields.
	if _, err := s.db.Exec(ctx, `UPDATE users SET last_seen_at = $1 WHERE id = $2`,
		time.Now().Add(-24*time.Hour), u.ID); err != nil {
		t.Fatalf("backdate last_seen_at: %v", err)
	}
	u3, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|preserve", Email: "changed@x.io", DisplayName: "Alice"})
	if err != nil {
		t.Fatalf("UpsertUser(stale repeat): %v", err)
	}
	if u3.DeactivatedAt == nil {
		t.Fatal("UpsertUser's write path (stale last_seen_at) cleared deactivated_at; " +
			"a deactivated user's next request must not silently revive them")
	}

	got, err := s.GetUserBySubject(ctx, tn.ID, "kc|preserve")
	if err != nil {
		t.Fatalf("GetUserBySubject: %v", err)
	}
	if got.DeactivatedAt == nil {
		t.Fatal("deactivated_at was cleared on the persisted row")
	}
}

// TestCountActiveUsers seeds three users at three different last_seen_at
// values (fresh, borderline-old, long-idle) and checks the count against
// cutoffs on both sides of the borderline row, so a boundary condition
// (`>` vs `>=`) is exercised rather than just "some rows in, some out".
// Cross-tenant isolation is checked too: a user in another tenant must never
// be counted for this one, mirroring every other tenant-scoped store query.
func TestCountActiveUsers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)
	other := mustTenant(t, s)

	fresh, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|fresh", Email: "fresh@x.io"})
	if err != nil {
		t.Fatalf("upsert fresh: %v", err)
	}
	borderline, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|borderline", Email: "b@x.io"})
	if err != nil {
		t.Fatalf("upsert borderline: %v", err)
	}
	idle, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|idle", Email: "idle@x.io"})
	if err != nil {
		t.Fatalf("upsert idle: %v", err)
	}
	if _, err := s.UpsertUser(ctx, User{TenantID: other.ID, Subject: "kc|other-tenant", Email: "o@x.io"}); err != nil {
		t.Fatalf("upsert other-tenant user: %v", err)
	}

	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	borderlineAt := cutoff.Add(time.Minute) // just inside the window
	idleAt := cutoff.Add(-time.Minute)      // just outside the window

	setLastSeen := func(id string, ts time.Time) {
		t.Helper()
		if _, err := s.db.Exec(ctx, `UPDATE users SET last_seen_at = $1 WHERE id = $2`, ts, id); err != nil {
			t.Fatalf("set last_seen_at for %s: %v", id, err)
		}
	}
	setLastSeen(fresh.ID, time.Now())
	setLastSeen(borderline.ID, borderlineAt)
	setLastSeen(idle.ID, idleAt)

	n, err := s.CountActiveUsers(ctx, tn.ID, cutoff)
	if err != nil {
		t.Fatalf("CountActiveUsers: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountActiveUsers = %d, want 2 (fresh + borderline; idle is outside the window, other-tenant is a different tenant)", n)
	}
}

// TestCountActiveUsersExcludesDeactivatedUsers is audit B9's sub-defect 1
// red-proof: a SCIM-deactivated user (or any user with deactivated_at set)
// must stop counting as an active seat IMMEDIATELY, not merely once
// last_seen_at eventually ages out of the 7-day window on its own. Both rows
// here carry an identical, fresh last_seen_at -- the only difference is
// deactivated_at -- so a boundary-only fix (last_seen_at alone) cannot pass
// this by accident.
func TestCountActiveUsersExcludesDeactivatedUsers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	active, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|still-active", Email: "a@x.io"})
	if err != nil {
		t.Fatalf("upsert active: %v", err)
	}
	deprovisioned, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|deprovisioned", Email: "d@x.io"})
	if err != nil {
		t.Fatalf("upsert deprovisioned: %v", err)
	}
	if err := s.DeactivateUser(ctx, tn.ID, deprovisioned.ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	n, err := s.CountActiveUsers(ctx, tn.ID, cutoff)
	if err != nil {
		t.Fatalf("CountActiveUsers: %v", err)
	}
	if n != 1 {
		t.Fatalf("CountActiveUsers = %d, want 1 (only %q; %q was just deactivated and must not still burn a seat "+
			"for the rest of the 7-day window)", n, active.Subject, deprovisioned.Subject)
	}
}

// TestUpsertProvisionedUserDoesNotConsumeASeatOnCreate is audit B9's
// sub-defect 2 red-proof: a brand-new row created via UpsertProvisionedUser
// (SCIM create's path) must not count as an active seat the instant it
// lands -- it has never authenticated. TestCountActiveUsers above already
// proves UpsertUser DOES make a fresh row count (that IS the authentication
// event for that path), so the two tests together pin the actual
// distinction rather than one absolute claim.
func TestUpsertProvisionedUserDoesNotConsumeASeatOnCreate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	if _, err := s.UpsertProvisionedUser(ctx, User{TenantID: tn.ID, Subject: "kc|scim-provisioned", Email: "p@x.io", DisplayName: "P"}); err != nil {
		t.Fatalf("UpsertProvisionedUser: %v", err)
	}

	// since = the dawn of the epoch: if the row's last_seen_at were anything
	// resembling "now" (the pre-fix DEFAULT), it would count against ANY
	// cutoff including this one. Only NULL (migration 00035's "never
	// authenticated") survives this assertion: `last_seen_at > since`
	// compares false against every possible since, by SQL's own
	// three-valued logic, regardless of how far in the past since is.
	n, err := s.CountActiveUsers(ctx, tn.ID, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("CountActiveUsers: %v", err)
	}
	if n != 0 {
		t.Fatalf("CountActiveUsers = %d, want 0: a SCIM-provisioned user who has never authenticated must not "+
			"consume a Community seat", n)
	}

	// The person's first REAL login (authz.Resolver.Resolve's own write
	// path) must still make them count -- proving this isn't a row that can
	// never count at all, only one that starts out not counting.
	if _, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|scim-provisioned", Email: "p@x.io", DisplayName: "P"}); err != nil {
		t.Fatalf("UpsertUser (first real login): %v", err)
	}
	n, err = s.CountActiveUsers(ctx, tn.ID, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("CountActiveUsers after first login: %v", err)
	}
	if n != 1 {
		t.Fatalf("CountActiveUsers after first real login = %d, want 1", n)
	}
}

// TestUpsertProvisionedUserDoesNotResetLastSeenOnUpdate is audit B9's
// sub-defect 3 red-proof: a SCIM displayName PATCH (UpsertProvisionedUser's
// UPDATE branch) on a user who HAS genuinely authenticated must not reset
// their activity clock. A user is seeded via the real UpsertUser
// (authentication) path, backdated to just inside the active window, then
// updated via UpsertProvisionedUser with a changed display name; last_seen_at
// must be byte-identical to the backdated value afterward, and the user must
// fall OUT of the active count once the same backdated timestamp is finally
// outside the window -- proving the PATCH truly never touched the column,
// not merely that it happened to still be "close enough".
func TestUpsertProvisionedUserDoesNotResetLastSeenOnUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	u, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|real-login-then-scim-edit", Email: "e@x.io", DisplayName: "Old Name"})
	if err != nil {
		t.Fatalf("seed real login: %v", err)
	}
	backdated := time.Now().Add(-6 * 24 * time.Hour) // inside the 7-day window, but not "now"
	if _, err := s.db.Exec(ctx, `UPDATE users SET last_seen_at = $1 WHERE id = $2`, backdated, u.ID); err != nil {
		t.Fatalf("backdate last_seen_at: %v", err)
	}

	if _, err := s.UpsertProvisionedUser(ctx, User{TenantID: tn.ID, Subject: u.Subject, Email: u.Email, DisplayName: "New Name (via SCIM)"}); err != nil {
		t.Fatalf("UpsertProvisionedUser (SCIM displayName patch): %v", err)
	}

	got := userLastSeenAt(t, s, u.ID)
	if !got.Equal(backdated) {
		t.Fatalf("last_seen_at after a SCIM displayName patch = %s, want unchanged %s: the patch must never "+
			"refresh the activity clock", got, backdated)
	}

	stored, err := s.GetUserByID(ctx, tn.ID, u.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if stored.DisplayName != "New Name (via SCIM)" {
		t.Fatalf("displayName = %q, want the SCIM-patched value (the write itself must still take effect)", stored.DisplayName)
	}

	// Now push the SAME backdated value just outside the 7-day window and
	// confirm the count excludes them despite the intervening SCIM patch --
	// the decisive proof that the patch did not silently keep the seat alive.
	cutoff := backdated.Add(time.Hour)
	n, err := s.CountActiveUsers(ctx, tn.ID, cutoff)
	if err != nil {
		t.Fatalf("CountActiveUsers: %v", err)
	}
	if n != 0 {
		t.Fatalf("CountActiveUsers = %d, want 0: the SCIM patch must not have refreshed last_seen_at past cutoff %s", n, cutoff)
	}
}

// TestDeleteUserReportsIdentity mirrors DeleteRole's
// TestDeleteRoleReportsWhatTheCascadeRevoked in shape (delete, check what
// came back, check the row is actually gone), but a user row has nothing
// downstream to cascade, so what it checks is the identity DeleteUser
// reports rather than a revoked-grants count (see DeletedUser's doc comment).
func TestDeleteUserReportsIdentity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	u, err := s.UpsertUser(ctx, User{
		TenantID: tn.ID, Subject: "kc|del", Email: "del@x.io", DisplayName: "Delete Me",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.DeleteUser(ctx, tn.ID, u.ID)
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if got.Subject != "kc|del" || got.Email != "del@x.io" || got.DisplayName != "Delete Me" {
		t.Fatalf("DeleteUser reported = %+v, want the deleted identity", got)
	}

	if _, err := s.GetUserBySubject(ctx, tn.ID, "kc|del"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected the row to be gone, got err=%v", err)
	}
}

// TestDeleteUserNotFound covers all three shapes that must 404: unknown id,
// malformed id, and cross-tenant id (a real user, but in a different
// tenant), and checks the cross-tenant user still exists afterwards, mirroring
// TestDeleteRoleNotFound.
func TestDeleteUserNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)
	other := mustTenant(t, s)

	foreign, err := s.UpsertUser(ctx, User{TenantID: other.ID, Subject: "kc|foreign", Email: "f@x.io"})
	if err != nil {
		t.Fatalf("upsert foreign user: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"unknown uuid", "00000000-0000-0000-0000-000000000000"},
		{"malformed id", "not-a-uuid"},
		{"cross-tenant id", foreign.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.DeleteUser(ctx, tn.ID, tc.id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("DeleteUser(%s) = %v, want ErrNotFound", tc.name, err)
			}
		})
	}

	if _, err := s.GetUserBySubject(ctx, other.ID, "kc|foreign"); err != nil {
		t.Fatalf("expected the foreign user to still exist after the cross-tenant 404, got err=%v", err)
	}
}

// TestDeleteUserCascadesDeploymentRows is G11: DELETE /v1/admin/users/{id} is
// the erasure path for the deployment registry's records about one person
// (migration 00017), and this asserts the schema actually makes it one.
//
// It lives here rather than beside the registry's own tests, and it seeds the
// row with raw SQL rather than through store.ReplaceDeployments, for one
// reason: the cascade is a property of the SCHEMA, which both editions ship,
// while the registry's store layer is Enterprise-only (artifact_deployment.ee.go
// is dropped from a generated Community tree by filename). A test written
// against the ee store functions would silently stop covering the Community
// build of a delete that behaves identically there.
//
// Two users, and the assertion names BOTH: without the second, a cascade
// implemented as "delete every deployment row" would pass.
func TestDeleteUserCascadesDeploymentRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	alice, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|cascade-alice", Email: "a@x.io"})
	if err != nil {
		t.Fatalf("upsert alice: %v", err)
	}
	bob, err := s.UpsertUser(ctx, User{TenantID: tn.ID, Subject: "kc|cascade-bob", Email: "b@x.io"})
	if err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	art, err := s.CreateArtifact(ctx, Artifact{TenantID: tn.ID, Type: "skill", Name: "cascade", Content: "body"})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	for _, u := range []User{alice, bob} {
		if _, err := s.db.Exec(ctx, `
			INSERT INTO artifact_deployment (tenant_id, user_id, install_id, artifact_id, revision)
			VALUES ($1, $2, gen_random_uuid(), $3, 3)`, tn.ID, u.ID, art.ID); err != nil {
			t.Fatalf("seed deployment row for %s: %v", u.Subject, err)
		}
	}

	countFor := func(userID string) int {
		var n int
		if err := s.db.QueryRow(ctx,
			`SELECT count(*)::int FROM artifact_deployment WHERE user_id = $1`, userID).Scan(&n); err != nil {
			t.Fatalf("count deployment rows: %v", err)
		}
		return n
	}
	if countFor(alice.ID) != 1 || countFor(bob.ID) != 1 {
		t.Fatalf("fixture is wrong: alice=%d bob=%d rows before the delete, want 1 each",
			countFor(alice.ID), countFor(bob.ID))
	}

	if _, err := s.DeleteUser(ctx, tn.ID, alice.ID); err != nil {
		t.Fatalf("DeleteUser: %v (without ON DELETE CASCADE on artifact_deployment.user_id "+
			"this is a 23503 foreign-key violation, i.e. a 500 on the erasure path)", err)
	}

	if n := countFor(alice.ID); n != 0 {
		t.Errorf("alice still has %d deployment row(s) after her user row was deleted; "+
			"they are a per-person data remnant whose owner no longer exists", n)
	}
	if n := countFor(bob.ID); n != 1 {
		t.Errorf("bob has %d deployment row(s) after alice was deleted, want 1: "+
			"the cascade must be scoped to the deleted user", n)
	}
}
