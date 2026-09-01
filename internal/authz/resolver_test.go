package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/migrate"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

var testDSN string

func TestMain(m *testing.M) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"), tcpostgres.WithUsername("orbeat"), tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-authz-tests"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				// Postgres opens the port, then restarts during init; wait for the
				// readiness log to appear twice so we don't connect mid-restart.
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
			),
		),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := migrate.Up(db); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = db.Close()
	testDSN = dsn
	code := m.Run()
	_ = pg.Terminate(ctx)
	os.Exit(code)
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	// Seed: tenant "default" + a role that matches a token role.
	tn, err := s.GetOrCreateTenantByName(ctx, "default")
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	adminRole, _ := s.CreateRole(ctx, tn.ID, "orbeat-admin")

	r := NewResolver(s, "default")
	rc, err := r.Resolve(ctx, auth.Principal{Subject: "kc-1", Email: "a@x.io", Roles: []string{"orbeat-admin", "unknown-role"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if rc.TenantID != tn.ID {
		t.Fatalf("tenant = %s, want %s", rc.TenantID, tn.ID)
	}
	if rc.UserID == "" {
		t.Fatal("expected user id")
	}
	if len(rc.RoleIDs) != 1 || rc.RoleIDs[0] != adminRole.ID {
		t.Fatalf("RoleIDs = %v, want [%s] (unknown-role skipped)", rc.RoleIDs, adminRole.ID)
	}

	// Idempotent: same subject keeps the same user id.
	rc2, _ := r.Resolve(ctx, auth.Principal{Subject: "kc-1", Email: "a@x.io"})
	if rc2.UserID != rc.UserID {
		t.Fatalf("user id not stable: %s vs %s", rc.UserID, rc2.UserID)
	}
}

// TestResolveSteadyStateNoWrite pins audit B4 at the Resolver layer (not just
// the underlying store functions): once a tenant+user exist and nothing about
// the principal changed, a repeat Resolve of the SAME principal must not
// rewrite either row.
//
// The users half of that is narrower than it sounds, and the narrowing is why
// it still passes. The two Resolve calls below are milliseconds apart, so
// last_seen_at is nowhere near internal/store's one-hour
// lastSeenWriteThreshold and the staleness write reason cannot fire. What is
// pinned is therefore "no write WITHIN the staleness threshold", which is the
// property audit B4 was about; a Resolve an hour later legitimately rewrites
// the users row, and this test would be wrong to forbid it. The tenant half
// has no such reason and is unqualified: GetOrCreateTenantByName writes only
// when the row is absent.
//
// xmin (the Postgres system column recording the
// inserting/updating transaction) is a direct, cheap change-detector: it
// advances on every UPDATE, including a no-op one, so xmin stability proves
// "no write happened," not merely "the visible fields look the same." Before
// the B4 fix this test fails on both rows: Resolve's tenant/user upserts
// unconditionally rewrote them on every single call.
func TestResolveSteadyStateNoWrite(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	raw, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatalf("raw db: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	xmin := func(table, id string) string {
		t.Helper()
		var x string
		if err := raw.QueryRowContext(ctx, "SELECT xmin::text FROM "+table+" WHERE id = $1", id).Scan(&x); err != nil {
			t.Fatalf("xmin(%s, %s): %v", table, id, err)
		}
		return x
	}

	tenantName := "steady-" + t.Name()
	r := NewResolver(s, tenantName)
	p := auth.Principal{Subject: "kc-steady-1", Email: "steady@x.io", Roles: []string{"orbeat-admin"}}

	rc, err := r.Resolve(ctx, p)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	tenantXminBefore := xmin("tenant", rc.TenantID)
	userXminBefore := xmin("users", rc.UserID)

	rc2, err := r.Resolve(ctx, p)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if rc2.TenantID != rc.TenantID || rc2.UserID != rc.UserID {
		t.Fatalf("expected stable ids, got %+v then %+v", rc, rc2)
	}

	if got := xmin("tenant", rc.TenantID); got != tenantXminBefore {
		t.Fatalf("steady-state Resolve rewrote the tenant row: xmin %s -> %s", tenantXminBefore, got)
	}
	if got := xmin("users", rc.UserID); got != userXminBefore {
		t.Fatalf("steady-state Resolve rewrote the user row: xmin %s -> %s", userXminBefore, got)
	}

	// An email change on the SAME subject must still propagate and must still
	// advance the user row's xmin — proving write-avoidance doesn't swallow
	// real updates.
	changed := auth.Principal{Subject: "kc-steady-1", Email: "changed@x.io", Roles: []string{"orbeat-admin"}}
	rc3, err := r.Resolve(ctx, changed)
	if err != nil {
		t.Fatalf("third Resolve (email change): %v", err)
	}
	if rc3.UserID != rc.UserID {
		t.Fatalf("expected stable user id across an email change, got %s then %s", rc.UserID, rc3.UserID)
	}
	got, err := s.GetUserBySubject(ctx, rc.TenantID, "kc-steady-1")
	if err != nil {
		t.Fatalf("GetUserBySubject: %v", err)
	}
	if got.Email != "changed@x.io" {
		t.Fatalf("expected the email change to propagate, got %+v", got)
	}
	if xminAfterChange := xmin("users", rc.UserID); xminAfterChange == userXminBefore {
		t.Fatalf("expected an email change to rewrite the user row (xmin unchanged: %s)", xminAfterChange)
	}
}

// TestResolveRefusesDeactivatedUser is the whole point of SCIM in orbeat
// (docs/specs/2026-08-25-orbeat-scim-design.md sec 2). A deactivated person
// still holds a valid Keycloak token; if Resolve does not refuse them, SCIM
// deprovisioning is theatre.
//
// Red-proven by hand: commenting out Resolve's checkDeactivated call (the
// mutant "Resolve ignores the column") makes the `err == nil` branch below
// fire instead, i.e. this test fails with "a deactivated user was resolved".
func TestResolveRefusesDeactivatedUser(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	r := NewResolver(s, "deactivated-"+t.Name())
	p := auth.Principal{Subject: "kc-deact-1", Email: "d@x.io"}

	rc, err := r.Resolve(ctx, p)
	if err != nil {
		t.Fatalf("first Resolve (must succeed, seeds the row): %v", err)
	}
	if err := s.DeactivateUser(ctx, rc.TenantID, rc.UserID); err != nil {
		t.Fatalf("DeactivateUser: %v", err)
	}

	if _, err := r.Resolve(ctx, p); err == nil {
		t.Fatal("a deactivated user was resolved; their token still works and SCIM deprovisioning is theatre")
	} else {
		var de DeactivatedUserError
		if !errors.As(err, &de) {
			t.Fatalf("expected a DeactivatedUserError distinguishable via errors.As (a deactivated "+
				"user is a 403, not a 500), got %v (%T)", err, err)
		}
		if de.Subject != p.Subject {
			t.Fatalf("DeactivatedUserError.Subject = %q, want %q", de.Subject, p.Subject)
		}
	}
}

// TestResolveDeactivationSurvivesRepeatedResolve is gate 2 of Task 1
// (docs/plans/orbeat-scim-2026-08-25.md): "a deactivated user's next request
// must not silently revive them." Resolve is called three times after
// deactivation, not once, because the FIRST refusal proves nothing about
// whether that very call's own machinery quietly undid the deactivation on
// its way to refusing -- only a SECOND (and third) call, reading the
// database fresh each time, can show the state is durable rather than a
// one-shot check that a side effect immediately invalidates.
//
// Honest caveat, found while red-proving this test against the "UpsertUser
// clears deactivated_at" mutant: under THIS resolver's chosen design
// (checkDeactivated runs and short-circuits BEFORE UpsertUser is ever
// called -- see Resolve's doc comment), that mutant is UNREACHABLE from
// Resolve entirely once a user is known-deactivated, because Resolve never
// reaches UpsertUser for such a subject again. This test therefore does NOT
// catch that mutant (verified by hand: applying it, this test still passes).
// The mutant IS caught, at the store layer where UpsertUser's write path
// actually executes, by store.TestUpsertUserPreservesDeactivation
// (internal/store/user_test.go), which calls UpsertUser directly on an
// already-deactivated row. Kept here anyway because it pins a real property
// Resolve must have on its own merits: repeated resolution of a deactivated
// subject stays refused, which a caching bug or an accidental early-return
// swallowing the error on retry could otherwise break silently.
func TestResolveDeactivationSurvivesRepeatedResolve(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	r := NewResolver(s, "deactivated-repeat-"+t.Name())
	p := auth.Principal{Subject: "kc-deact-repeat", Email: "dr@x.io"}

	rc, err := r.Resolve(ctx, p)
	if err != nil {
		t.Fatalf("seed Resolve: %v", err)
	}
	if err := s.DeactivateUser(ctx, rc.TenantID, rc.UserID); err != nil {
		t.Fatalf("DeactivateUser: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := r.Resolve(ctx, p); err == nil {
			t.Fatalf("Resolve attempt %d after deactivation succeeded; the deactivation did not survive", i)
		}
	}
}
