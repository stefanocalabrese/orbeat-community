package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// EntitlementChannel is the Postgres LISTEN/NOTIFY channel carrying "something
// a gateway session snapshotted has changed for this tenant". The payload is
// the tenant id and nothing else: a nudge to rebuild, never data.
//
// That framing is the whole safety argument. A gateway that misses every
// notification still behaves exactly as it did before this existed, because
// sessionMaxAge already rebuilds within five minutes. Nothing here may become
// load-bearing: no caller waits for a notification, no decision depends on one
// arriving, and a listener that dies costs latency rather than correctness.
const EntitlementChannel = "orbeat_entitlement_change"

// NotifyEntitlementChange announces that tenantID's entitlements, roles or
// servers changed. Called INSIDE the mutating transaction: Postgres delivers a
// NOTIFY only on commit, so a rolled-back change cannot nudge anyone, and a
// committed one cannot fail to.
//
// Errors are returned rather than swallowed so a caller inside InTx can decide.
// Every caller today logs and continues: failing an admin's write because a
// performance hint could not be queued would be the tail wagging the dog.
func (s *Store) NotifyEntitlementChange(ctx context.Context, tenantID string) error {
	// pg_notify rather than NOTIFY: the channel is a constant but the payload
	// is data, and NOTIFY takes no parameters, so the alternative is string
	// concatenation into SQL.
	if _, err := s.db.Exec(ctx, `SELECT pg_notify($1, $2)`, EntitlementChannel, tenantID); err != nil {
		return fmt.Errorf("notify entitlement change: %w", err)
	}
	return nil
}

// ErrNoPool is returned by Listen on a tx-bound Store, which has no pool to
// take a dedicated connection from. Listening inside a transaction would also
// be wrong: the connection would be held for the transaction's life.
var ErrNoPool = errors.New("listen: this Store is transaction-bound and has no pool")

// Listen holds ONE dedicated pooled connection on channel and calls onNotify
// for each payload until ctx ends or the connection breaks. It returns the
// error that ended it so the caller can decide whether to reconnect; a caller
// that reconnects must back off, since the common cause is the database being
// unreachable.
//
// A dedicated connection is not a preference. LISTEN is connection-scoped
// state, so a pooled connection returned between calls would silently stop
// listening the moment the pool handed it to someone else.
func (s *Store) Listen(ctx context.Context, channel string, onNotify func(payload string)) error {
	if s.pool == nil {
		return ErrNoPool
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("listen: acquire: %w", err)
	}
	// Hijack, NOT Release, and the difference is a reproduced defect rather
	// than a preference. pgxpool.Release destroys a connection only when it is
	// closed, busy, mid-transaction or past its lifetime (pgxpool/conn.go);
	// otherwise it hands it straight back to the pool. Every path out of this
	// function leaves the connection healthy and idle, so Release returned a
	// still-subscribed connection to the pool. Measured on both cancellation
	// paths, and an idle pooled connection reads nothing, so Postgres keeps
	// queueing notifications for a listener that will never consume them while
	// StartEntitlementNudge's reconnect loop adds another on every blip.
	//
	// Hijack removes the connection from the pool permanently, which is what
	// the invariant above actually requires, and makes closing it this
	// function's job.
	defer func() {
		raw := conn.Hijack()
		// The caller's context is already cancelled on the ordinary shutdown
		// path, so closing under it would abandon the socket instead of
		// terminating the session cleanly.
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelClose()
		_ = raw.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+quoteIdent(channel)); err != nil {
		return fmt.Errorf("listen %s: %w", channel, err)
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		onNotify(n.Payload)
	}
}

// quoteIdent double-quotes an identifier for interpolation into SQL that
// cannot take a parameter (LISTEN takes none). Callers pass constants today;
// quoting is here so that stops being load-bearing.
func quoteIdent(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, s[i])
	}
	return string(append(out, '"'))
}
