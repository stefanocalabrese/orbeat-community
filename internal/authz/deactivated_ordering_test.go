package authz

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// lastSeenOf reads users.last_seen_at directly. store.User does not carry the
// column, and the ordering this file gates is observable ONLY through it.
func lastSeenOf(t *testing.T, pool *pgxpool.Pool, userID string) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT last_seen_at FROM users WHERE id = $1`, userID).Scan(&ts); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	return ts
}

func backdateLastSeen(t *testing.T, pool *pgxpool.Pool, userID string, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET last_seen_at = now() - $2::interval WHERE id = $1`,
		userID, d.String()); err != nil {
		t.Fatalf("backdate last_seen_at: %v", err)
	}
}

// TestResolveRefusesDeactivatedUserBeforeBumpingLastSeen closes audit finding C2.
//
// Resolve's own doc comment calls this ordering "the load-bearing part":
// checkDeactivated must precede UpsertUser, because refusing first is what stops
// a deactivated person's requests from bumping last_seen_at and so holding a
// Community active-seat slot they no longer legitimately occupy.
//
// NOTHING COULD FAIL FOR IT. The audit red-proved that moving checkDeactivated
// to run AFTER UpsertUser leaves both internal/authz and internal/api green: the
// deactivation still refuses, so every existing test still sees its error, and
// not one of them looks at the write that happened on the way. Under that
// mutant store/user.go bumps last_seen_at hourly on a deactivated row and
// seatcap.go counts it as an active seat for seven days, so a deprovisioned
// contractor holds a Community seat indefinitely — on the exact surface
// deactivation exists to free.
//
// So this asserts the SIDE EFFECT, not the refusal. The refusal is already
// covered by TestResolveRefusesDeactivatedUser and
// TestMiddlewareRefusesDeactivatedUserWith403, and covering it again here would
// reproduce the gap rather than close it.
func TestResolveRefusesDeactivatedUserBeforeBumpingLastSeen(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	r := NewResolver(s, "deact-order-"+t.Name())
	p := auth.Principal{Subject: "deact-order-1", Email: "d@x.io"}
	rc, err := r.Resolve(ctx, p)
	if err != nil {
		t.Fatalf("seed the user: %v", err)
	}

	// Backdate well past store's lastSeenWriteThreshold (1 hour), or UpsertUser
	// legitimately skips the write and this test would pass on the mutant too.
	backdateLastSeen(t, pool, rc.UserID, 48*time.Hour)

	if err := s.DeactivateUser(ctx, rc.TenantID, rc.UserID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	before := lastSeenOf(t, pool, rc.UserID)
	if _, err := r.Resolve(ctx, p); err == nil {
		t.Fatal("Resolve accepted a deactivated user")
	}
	after := lastSeenOf(t, pool, rc.UserID)

	if !after.Equal(before) {
		t.Errorf(`Resolve refused the deactivated user but bumped last_seen_at on the way
(%v -> %v), so checkDeactivated is running AFTER UpsertUser.

Resolve's doc comment calls this ordering the load-bearing part, and it is: with the
bump, seatcap.go counts this deprovisioned user as an active seat for the full 7-day
window, so a departed contractor keeps occupying a Community seat that deactivation
was supposed to free. The refusal alone is not the guarantee — every other test in
this package already covers that and every one of them passes with the order flipped.

Move checkDeactivated back above UpsertUser in Resolve (resolver.go).`, before, after)
	}
}

// TestResolveDoesBumpLastSeenForAnActiveUser is the non-vacuity half, and
// without it the test above is worthless.
//
// That test proves last_seen_at did not move. It would pass just as well if
// UpsertUser had stopped writing last_seen_at at ALL — if the threshold logic
// broke, if the column stopped being maintained, if the backdate silently
// failed. Then the seat window would never refresh for anyone, every active
// user would drop out of the count after 7 days, and the gate next door would
// still be green while reporting that deactivation works.
//
// This pins the other direction on the identical setup: same backdating, same
// reads, the only difference being that the user is not deactivated.
func TestResolveDoesBumpLastSeenForAnActiveUser(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	r := NewResolver(s, "deact-order-active-"+t.Name())
	p := auth.Principal{Subject: "deact-order-active-1", Email: "a@x.io"}
	rc, err := r.Resolve(ctx, p)
	if err != nil {
		t.Fatalf("seed the user: %v", err)
	}

	backdateLastSeen(t, pool, rc.UserID, 48*time.Hour)
	before := lastSeenOf(t, pool, rc.UserID)

	if _, err := r.Resolve(ctx, p); err != nil {
		t.Fatalf("Resolve refused an ACTIVE user: %v", err)
	}
	after := lastSeenOf(t, pool, rc.UserID)

	if !after.After(before) {
		t.Errorf(`Resolve did not bump last_seen_at for an ACTIVE user whose row was 48h stale
(%v -> %v).

This makes TestResolveRefusesDeactivatedUserBeforeBumpingLastSeen vacuous: it proves
last_seen_at did not move, which is trivially true if last_seen_at never moves for
anyone. It also means the Community seat window never refreshes, so every active user
silently drops out of the count after 7 days.`, before, after)
	}
}
