package authz

import (
	"context"
	"database/sql"
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
// rewrite either row. xmin (the Postgres system column recording the
// inserting/updating transaction) is a direct, cheap change-detector — it
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
