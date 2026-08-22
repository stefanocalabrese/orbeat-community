// Package store provides typed access to orbeat's Postgres data.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a requested row does not exist (or is not visible
// to the given tenant). Callers can map it to HTTP 404 via errors.Is.
var ErrNotFound = errors.New("store: not found")

// ErrVersionMismatch reports that a conditional update's expected row_version
// did not match the row's current value — someone else wrote first. The API
// layer maps it to 412.
var ErrVersionMismatch = errors.New("version mismatch")

// idCastNotFound reports whether err indicates "no such row" for a lookup
// whose only USER-SUPPLIED cast is the uuid id parameter (an internal,
// trusted value like tenantID may also be uuid-cast in the same query — that
// doesn't matter, since it can never fail): either no row matched
// (pgx.ErrNoRows) or the id itself failed the uuid cast (Postgres 22P02,
// invalid_text_representation — a malformed id can never match an existing
// row, so "not found" is the correct outcome either way). Do not reuse this
// helper for a query with any OTHER user-supplied cast: it would then mask an
// unrelated invalid-input error as a plain not-found. Verify this holds for
// each call site individually before applying it.
func idCastNotFound(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// dbConn is the subset of pgx used by the store. Both *pgxpool.Pool and pgx.Tx
// satisfy it, so every method works inside or outside a transaction unchanged.
type dbConn interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is a typed handle over a Postgres connection (a pool, or a tx inside InTx).
// pool is non-nil only for a top-level Store; it is nil for a transaction-bound Store
// created by InTx. InTx and Close operate on the top-level Store only.
type Store struct {
	pool *pgxpool.Pool // nil for tx-bound Stores; non-nil for top-level Stores only
	db   dbConn        // pool at top level, pgx.Tx inside InTx
}

// New opens a connection pool to dbURL (no query tracing) and verifies connectivity.
func New(ctx context.Context, dbURL string) (*Store, error) {
	return NewWithTracer(ctx, dbURL, nil)
}

// NewWithTracer opens a pool with an optional pgx query tracer (nil = none) and
// verifies connectivity. cmd/api and cmd/gateway pass the telemetry tracer; tests
// and other callers use New (nil tracer).
func NewWithTracer(ctx context.Context, dbURL string, tracer pgx.QueryTracer) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if tracer != nil {
		cfg.ConnConfig.Tracer = tracer
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool, db: pool}, nil
}

// PoolStat returns pool statistics for metrics gauges, or nil for a tx-bound Store.
func (s *Store) PoolStat() *pgxpool.Stat {
	if s.pool == nil {
		return nil
	}
	return s.pool.Stat()
}

// Close releases the pool. It is a no-op on a transaction-bound Store.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// InTx runs fn inside a single transaction. The Store passed to fn issues all of
// its queries on that transaction. If fn returns an error (or panics) the
// transaction is rolled back; otherwise it commits. Use this to make a mutation
// and its audit row atomic (fail-closed auditing).
func (s *Store) InTx(ctx context.Context, fn func(tx *Store) error) (err error) {
	if s.pool == nil {
		return errors.New("store: InTx called on a transaction-bound Store (nesting is not supported)")
	}
	pgtx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = pgtx.Rollback(ctx)
			panic(p)
		}
	}()
	// Deliberately a fresh struct literal, NOT a copy of the receiver (cp := *s;
	// cp.db = pgtx) — a receiver copy would carry every future field into the
	// transaction by default with no way to ever catch one that shouldn't cross,
	// and it would make TestInTxPropagatesEveryStoreField (store_tx_test.go)
	// unfalsifiable: both sides of that test's comparison would derive the child the
	// same way and agree by construction, forever. See
	// docs/specs/2026-08-19-orbeat-intx-field-propagation-design.md §2. Adding a
	// field to Store? That test will fail and tell you what to do.
	if err = fn(&Store{db: pgtx}); err != nil {
		_ = pgtx.Rollback(ctx)
		return err
	}
	if err = pgtx.Commit(ctx); err != nil {
		_ = pgtx.Rollback(ctx) // best-effort cleanup; no-op if already closed
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
