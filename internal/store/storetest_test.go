package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stefanocalabrese/orbeat-community/internal/migrate"
)

var testDSN string

// TestMain starts one Postgres container for the whole package, runs
// migrations, exposes the DSN via testDSN, then runs the tests.
func TestMain(m *testing.M) {
	ctx := context.Background()

	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"),
		tcpostgres.WithUsername("orbeat"),
		tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-store-tests"),
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
		fmt.Fprintf(os.Stderr, "start postgres: %v\n", err)
		os.Exit(1)
	}

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		os.Exit(1)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	if err := migrate.Up(db); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
	_ = db.Close()

	testDSN = dsn
	code := m.Run()
	_ = pg.Terminate(ctx)
	os.Exit(code)
}

// newTestStore opens a Store against the shared container.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(context.Background(), testDSN)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// mustTenant creates a fresh tenant for test isolation.
func mustTenant(t *testing.T, s *Store) Tenant {
	t.Helper()
	name := fmt.Sprintf("t-%d", time.Now().UnixNano())
	tn, err := s.CreateTenant(context.Background(), name)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return tn
}

// seqName builds a distinct per-iteration name, e.g. seqName("srv", 3) ==
// "srv-3". Plain decimal via strconv, unlike a prior version of this helper
// that mapped i onto a rune offset from 'a': that scheme broke past i=25
// (non-letter runes), and collided outright in the UTF-16 surrogate range
// (e.g. i=55199 and i=55200 both produced the same U+FFFD replacement
// character), which would have surfaced as a baffling unique-constraint
// failure rather than a readable name mismatch.
func seqName(prefix string, i int) string {
	return prefix + "-" + strconv.Itoa(i)
}

// explain runs plain EXPLAIN — deliberately no ANALYZE, no VERBOSE — on the
// exact SQL+args a …PageSQL builder produced and returns the plan text (one
// line per row, newline-joined). It takes the built SQL rather than
// reconstructing the query, so the test cannot drift from what production
// actually runs — a hand-rewritten approximation could pass or fail for
// reasons unrelated to the real query.
//
// Plain EXPLAIN, not EXPLAIN ANALYZE: these tests assert plan SHAPE (which
// index/node the planner chose, whether a Sort node appears), which a plain
// EXPLAIN reports deterministically from the planner's cost estimates alone.
// ANALYZE would additionally execute the query and report real timings —
// unnecessary for a shape assertion, and it trades determinism for noise
// (actual runtime varies run to run; the chosen plan does not, given stable
// statistics from a preceding ANALYZE of the table).
//
// No VERBOSE, and that omission is LOAD-BEARING, not an oversight: VERBOSE
// adds an `Output: …` line per node listing the target list, and this
// query's SELECT list always contains the literal expression `(id)::text`
// (id::text is how every row is projected, qualified or not — that's the
// projection, unrelated to the ORDER BY defect this package tests for).
// Verified directly: `EXPLAIN (VERBOSE)` on the FIXED query still prints
// `Output: ((id)::text), …`, which would make every `strings.Contains(plan,
// "(id)::text")` assertion in this package a false positive — passing before
// the fix and failing after it, backwards from what it's for. If a future
// change to these tests wants column-level detail from VERBOSE, it cannot
// reuse the `(id)::text`-absence check as written; it needs a different,
// sort-node-scoped signal (e.g. matching only within a `Sort Key:` line).
func explain(t *testing.T, s *Store, sql string, args ...any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v\nSQL: %s\nargs (%d): %#v", err, sql, len(args), args)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return b.String()
}

// isConstraintViolation reports whether err came from the named Postgres
// constraint or index. Matching the NAME, not just the SQLSTATE, is what makes
// the assertions built on it discriminating: artifact carries several CHECKs
// and several unique keys, so "some integrity constraint fired" would pass on
// the wrong one and read as proof of the right one. errors.As reaches through
// store.transition, which wraps the pgx error with %w.
func isConstraintViolation(err error, name string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == name
}
