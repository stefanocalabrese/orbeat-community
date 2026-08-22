package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditEvent records an authorization or runtime decision.
// Decision must be one of: allow, deny, error.
type AuditEvent struct {
	ID       string
	TenantID string
	TS       time.Time
	Actor    string
	Action   string
	Target   string
	Decision string
	Metadata map[string]any
}

// AppendAuditEvent inserts an audit row and returns it with generated id and ts.
func (s *Store) AppendAuditEvent(ctx context.Context, e AuditEvent) (AuditEvent, error) {
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return AuditEvent{}, fmt.Errorf("marshal metadata: %w", err)
	}
	err = s.db.QueryRow(ctx, `
		INSERT INTO audit_event (tenant_id, actor, action, target, decision, metadata)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id::text, ts`,
		e.TenantID, e.Actor, e.Action, e.Target, e.Decision, raw,
	).Scan(&e.ID, &e.TS)
	if err != nil {
		return AuditEvent{}, fmt.Errorf("append audit event: %w", err)
	}
	e.Metadata = meta
	return e, nil
}

// ListAuditEventsByTenant returns up to limit events, newest first. It is the
// unpaginated convenience over ListAuditEventsPage (nil cursor → newest page).
func (s *Store) ListAuditEventsByTenant(ctx context.Context, tenantID string, limit int) ([]AuditEvent, error) {
	return s.ListAuditEventsPage(ctx, tenantID, nil, limit)
}

// AuditCursor is a keyset position into a tenant's audit log, ordered (ts, id)
// descending. Pass nil to start from the newest event.
type AuditCursor struct {
	TS time.Time
	ID string
}

// auditPageSQL builds the tenant-scoped keyset page query for the audit log
// and its COMPLETE bind-ordered argument list (tenantID included). Split out
// from ListAuditEventsPage so the index-usage test can EXPLAIN the exact
// SQL+args pair that runs in production, following the same
// SQL-plus-complete-args seam Task 2 established — for two of the other five
// paginated lists (mcpServerPageSQL, entitlementPageSQL) directly; the
// remaining three (role, artifact, artifact_entitlement) are later tasks in
// the same plan and do not compile as of this commit (68cfe5f) — but audit
// keeps its own (ts, id) cursor shape and its own 100/1000 limits rather than
// keysetTail (spec §2 deliberately excludes it from that rewrite).
//
// The sort keys are table-qualified (audit_event.ts, audit_event.id), and
// that qualification is load-bearing, not cosmetic: the projection selects
// id::text, so the output column is LABELLED id, and Postgres resolves a BARE
// name in ORDER BY against output labels before table columns. An unqualified
// `ORDER BY id` therefore sorts by that text cast, which
// audit_event_tenant_ts_id_idx (a uuid-typed index) cannot serve. The
// unqualified query itself shipped on 2026-06-10 (10a7266); migration 00010
// landed six weeks later, on 2026-07-21 (dfea40e), adding
// audit_event_tenant_ts_id_idx as (per its own comment) a fix for exactly
// this — the query was broken before that index existed, and what 00010
// actually added was an index the pre-existing defect made unusable. Neither
// was caught, because no test asserted a plan. See paging.go's keysetTail
// comment for the general mechanism.
//
// Unlike the dramatic (orders-of-magnitude) win the same defect class
// produces on entitlement's (role_id, id) keyset — see paging.go — audit's
// own win is small, shape-dependent, and in one common shape not a win at
// all; honestly measured here rather than borrowed from that unrelated
// query. orbeat's deployment is single-tenant-per-instance, so in production
// the leading tenant_id key filters out ~nothing, and at that shape (single
// tenant, chronological ts/insertion order — the realistic append pattern)
// Postgres picks the IDENTICAL plan node on both sides of this fix: the same
// Incremental-Sort-over-audit_event_ts_idx, with byte-identical estimated
// cost. Measured there (EXPLAIN ANALYZE, 100k rows, medians of 21+ reps): the
// qualified form is NOT faster — it comes out roughly flat to a few percent
// slower across LIMIT 100/1000/10000 (one measurement environment saw
// 0.91-0.94x; another saw 0.94-1.03x — both readings say the same thing:
// no speed win here, sometimes a small loss). `EXPLAIN (VERBOSE)` shows why:
// qualifying the ORDER BY makes Postgres carry an EXTRA output column
// (the native uuid `id`, alongside the already-projected `id::text`) through
// every node below the Limit, because the sort key is no longer able to
// reuse the text expression already in the SELECT list — one more 16-byte
// column moved through the sort than the unqualified plan needs. This fix is
// a correctness / index-eligibility fix at that shape, not a speed-up.
//
// What IS unconditionally true, and what TestAuditPageUsesKeysetIndex's
// first assertion pins, is that the id tiebreak ALWAYS falls out of the
// index before this fix (the query's plan always names the `::text` cast)
// and NEVER does after — regardless of table shape. The second assertion
// (that audit_event_tenant_ts_id_idx is actually driven with no Sort node at
// all) needs a shape where tenant_id is genuinely selective to be reliably
// true; see the seeding comment on the test itself. Where the composite
// index IS driven, real execution time does improve, by roughly 1.3-1.6x in
// the shapes measured — because that path removes the Incremental Sort node
// entirely, not because of a per-comparison saving.
//
// WHERE is unaffected (output labels are not visible there), so the (ts, id)
// cursor predicate below is fine unqualified.
func auditPageSQL(tenantID string, cursor *AuditCursor, limit int) (string, []any) {
	const base = `
		SELECT id::text, tenant_id::text, ts, actor, action, target, decision, metadata
		FROM audit_event
		WHERE tenant_id = $1`
	if cursor == nil {
		return base + `
			ORDER BY audit_event.ts DESC, audit_event.id DESC
			LIMIT $2`, []any{tenantID, limit}
	}
	return base + `
		AND (ts, id) < ($2, $3)
		ORDER BY audit_event.ts DESC, audit_event.id DESC
		LIMIT $4`, []any{tenantID, cursor.TS, cursor.ID, limit}
}

// ListAuditEventsPage returns up to limit events for a tenant, newest first,
// strictly older than cursor (by (ts, id)) when cursor is non-nil. The (ts, id)
// tiebreak gives a stable total order even when timestamps collide.
func (s *Store) ListAuditEventsPage(ctx context.Context, tenantID string, cursor *AuditCursor, limit int) ([]AuditEvent, error) {
	sql, args := auditPageSQL(tenantID, cursor, limit)
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit events page: %w", err)
	}
	defer rows.Close()
	return scanAuditEvents(rows)
}

// auditRangeSelect is the shared projection+filter for the chronological
// [from, to] export queries (nil bound = unbounded), ascending by (ts, id).
// Table-qualified for the same reason as auditPageSQL above: id is projected
// as id::text, so an unqualified ORDER BY id would sort by that text cast
// instead of the indexed uuid column.
const auditRangeSelect = `
	SELECT id::text, tenant_id::text, ts, actor, action, target, decision, metadata
	FROM audit_event
	WHERE tenant_id = $1
	  AND ($2::timestamptz IS NULL OR ts >= $2)
	  AND ($3::timestamptz IS NULL OR ts <= $3)
	ORDER BY audit_event.ts ASC, audit_event.id ASC
	LIMIT $4`

// ForEachAuditEventInRange streams audit events for a tenant within an optional
// [from, to] time window, chronological (ts, id) order, invoking fn per row
// WITHOUT materializing them into a slice — the audit export can be arbitrarily
// large, so the handler writes each row straight to the response. Iteration
// stops (and returns) as soon as fn returns an error. Capped at limit; limit <=
// 0 means unbounded — passed to Postgres as a nil $4, and `LIMIT NULL` means no
// limit at all (as opposed to LIMIT 0, which would return zero rows).
func (s *Store) ForEachAuditEventInRange(ctx context.Context, tenantID string, from, to *time.Time, limit int, fn func(AuditEvent) error) error {
	var limArg any = limit
	if limit <= 0 {
		limArg = nil
	}
	rows, err := s.db.Query(ctx, auditRangeSelect, tenantID, from, to, limArg)
	if err != nil {
		return fmt.Errorf("for each audit event in range: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanOneAuditEvent(rows)
		if err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ListAuditEventsInRange returns audit events for a tenant within an optional
// [from, to] time window (nil bound = unbounded), ascending by (ts, id) —
// chronological order for export/archival. Capped at limit. It is the
// materializing convenience over ForEachAuditEventInRange; the streaming export
// path uses ForEach directly.
func (s *Store) ListAuditEventsInRange(ctx context.Context, tenantID string, from, to *time.Time, limit int) ([]AuditEvent, error) {
	var out []AuditEvent
	err := s.ForEachAuditEventInRange(ctx, tenantID, from, to, limit, func(e AuditEvent) error {
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AuditEventsInRangeExceed reports whether MORE than `limit` audit events match
// the tenant + optional [from, to] window. The streaming export calls it to
// decide the X-Orbeat-Export-Truncated header BEFORE it writes the first body
// byte (an HTTP header cannot be set once the body starts, and the streaming
// path can't buffer the whole result to count it after the fact). It stops as
// soon as a (limit+1)-th matching row exists (LIMIT 1 OFFSET limit), so it never
// scans the whole window.
func (s *Store) AuditEventsInRangeExceed(ctx context.Context, tenantID string, from, to *time.Time, limit int) (bool, error) {
	var exceeds bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM audit_event
			WHERE tenant_id = $1
			  AND ($2::timestamptz IS NULL OR ts >= $2)
			  AND ($3::timestamptz IS NULL OR ts <= $3)
			-- No ORDER BY: this is an existence test for a (limit+1)-th matching
			-- row, which does not depend on ordering (do not add one to match
			-- the paginated select's shape — it would only add a needless sort).
			OFFSET $4
		)`, tenantID, from, to, limit).Scan(&exceeds)
	if err != nil {
		return false, fmt.Errorf("audit events in range exceed: %w", err)
	}
	return exceeds, nil
}

// PruneAuditEventsOlderThan deletes audit_event rows with ts < cutoff, in
// bounded batches of `batch` rows, and returns the total deleted. Postgres
// DELETE has no LIMIT, so each batch deletes by ctid from a bounded subquery —
// keeping every statement short so a large purge never long-locks the table.
// A batch <= 0 is treated as a sane default (10000).
func (s *Store) PruneAuditEventsOlderThan(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	if batch <= 0 {
		batch = 10000
	}
	var total int64
	for {
		tag, err := s.db.Exec(ctx, `
			DELETE FROM audit_event
			WHERE ctid IN (
				SELECT ctid FROM audit_event WHERE ts < $1 LIMIT $2
			)`, cutoff, batch)
		if err != nil {
			return total, fmt.Errorf("prune audit events: %w", err)
		}
		n := tag.RowsAffected()
		total += n
		if n < int64(batch) {
			break // last (partial) batch drained the backlog
		}
	}
	return total, nil
}

// scanOneAuditEvent scans a single audit row, decoding the jsonb metadata column.
func scanOneAuditEvent(rows pgx.Rows) (AuditEvent, error) {
	var e AuditEvent
	var raw []byte
	if err := rows.Scan(&e.ID, &e.TenantID, &e.TS, &e.Actor, &e.Action,
		&e.Target, &e.Decision, &raw); err != nil {
		return AuditEvent{}, fmt.Errorf("scan audit event: %w", err)
	}
	if err := json.Unmarshal(raw, &e.Metadata); err != nil {
		return AuditEvent{}, fmt.Errorf("unmarshal metadata: %w", err)
	}
	return e, nil
}

// scanAuditEvents materializes audit rows, decoding the jsonb metadata column.
func scanAuditEvents(rows pgx.Rows) ([]AuditEvent, error) {
	var out []AuditEvent
	for rows.Next() {
		e, err := scanOneAuditEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
