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
