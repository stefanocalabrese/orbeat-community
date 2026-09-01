package store

import (
	"context"
	"fmt"
)

// MCPServer is a catalog entry for an upstream MCP server.
// SecretRef is a provider reference (e.g. "vault:<mount>/<path>#<field>",
// "env:NAME", "awssm:<name-or-arn>#<jsonkey>"), never a raw secret.
type MCPServer struct {
	ID                string
	TenantID          string
	Name              string
	Description       string
	Transport         string // stdio | http | sse
	EndpointOrCommand string
	Version           string
	ProtocolVersion   string
	SecretRef         string // empty means none
	// TLSCARef references a CA certificate (PEM) to verify this upstream
	// against, INSTEAD of the system pool. Empty means the system pool.
	TLSCARef string
	Status   string

	// RowVersion is the optimistic-concurrency token. Incremented by the
	// mcp_server_bump_row_version trigger (migration 00013) on EVERY update, so
	// no statement can change the row without invalidating an outstanding
	// client's precondition.
	RowVersion int64
}

// CreateMCPServer inserts a catalog entry and returns the full persisted row
// (via mcpServerCols/scanMCPServer, not just the generated id) so the caller's
// RowVersion reflects the DB's actual default (1) rather than the Go
// zero-value — a create response that under-reports row_version would make
// the very first client precondition after create fail spuriously.
func (s *Store) CreateMCPServer(ctx context.Context, m MCPServer) (MCPServer, error) {
	created, err := scanMCPServer(s.db.QueryRow(ctx, `
		INSERT INTO mcp_server
			(tenant_id, name, description, transport, endpoint_or_command,
			 version, protocol_version, secret_ref, tls_ca_ref, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10)
		RETURNING `+mcpServerCols,
		m.TenantID, m.Name, m.Description, m.Transport, m.EndpointOrCommand,
		m.Version, m.ProtocolVersion, m.SecretRef, m.TLSCARef, m.Status,
	))
	if err != nil {
		return MCPServer{}, fmt.Errorf("create mcp_server: %w", err)
	}
	return created, nil
}

func scanMCPServer(row interface{ Scan(...any) error }) (MCPServer, error) {
	var m MCPServer
	var secretRef, tlsCARef *string
	err := row.Scan(
		&m.ID, &m.TenantID, &m.Name, &m.Description, &m.Transport,
		&m.EndpointOrCommand, &m.Version, &m.ProtocolVersion, &secretRef, &tlsCARef, &m.Status,
		&m.RowVersion,
	)
	if err != nil {
		return MCPServer{}, err
	}
	if secretRef != nil {
		m.SecretRef = *secretRef
	}
	if tlsCARef != nil {
		m.TLSCARef = *tlsCARef
	}
	return m, nil
}

const mcpServerCols = `id::text, tenant_id::text, name, description, transport,
	endpoint_or_command, version, protocol_version, secret_ref, tls_ca_ref, status, row_version`

// GetMCPServer fetches a catalog entry scoped to tenantID; a cross-tenant or
// unknown id returns ErrNotFound (the SQL filter makes this the only possible
// outcome, so a caller cannot forget the tenant check).
func (s *Store) GetMCPServer(ctx context.Context, tenantID, id string) (MCPServer, error) {
	m, err := scanMCPServer(s.db.QueryRow(ctx,
		`SELECT `+mcpServerCols+` FROM mcp_server WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if err != nil {
		if idCastNotFound(err) {
			return MCPServer{}, ErrNotFound
		}
		return MCPServer{}, fmt.Errorf("get mcp_server: %w", err)
	}
	return m, nil
}

// MCPServerExistsInTenant locks the server row (FOR SHARE) and reports whether
// it belongs to tenantID, so a caller can safely insert a referencing row. A
// malformed id (uuid cast failure) is reported the same as "doesn't exist"
// (false, nil) — it can never match a real row either way, and this
// function's non-existence signal is a bool, not an error.
func (s *Store) MCPServerExistsInTenant(ctx context.Context, tenantID, id string) (bool, error) {
	var got string
	err := s.db.QueryRow(ctx,
		`SELECT id::text FROM mcp_server WHERE tenant_id = $1 AND id = $2 FOR SHARE`,
		tenantID, id).Scan(&got)
	if err != nil {
		if idCastNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("mcp_server exists check: %w", err)
	}
	return true, nil
}

// mcpServerKeys is mcp_server's sort order (id appended by keysetTail).
var mcpServerKeys = []sortKey{{Col: "name", Cast: "text"}}

// MCPServerCursor is the keyset position just after m, walked in direction
// desc (?order), see RoleCursor's doc comment for why desc must match.
func MCPServerCursor(m MCPServer, desc bool) ListCursor {
	return ListCursor{Keys: []string{m.Name}, ID: m.ID, Sort: sortIdentity("mcp_server", mcpServerKeys, desc)}
}

// mcpServerPageSQL builds the tenant-scoped keyset page query and its
// COMPLETE argument list (tenantID included, in bind order). Split out from
// ListMCPServersPage so the index-usage test can EXPLAIN the exact SQL+args
// pair that runs in production, and so the caller never has to reconstruct
// the correspondence between the two by hand.
//
// $2 is the ?q= substring search filter (docs/plans/orbeat-admin-search-sort-
// 2026-08-27.md Task 4), the same NULL-means-no-filter shape rolePageSQL uses
// (rbac.go) and for the same reasons, see likeSearchArg's doc comment
// (paging.go). Applied in SQL before keysetTail's cursor predicate and LIMIT.
func mcpServerPageSQL(tenantID string, cursor *ListCursor, limit int, desc bool, search string) (string, []any, error) {
	const base = `SELECT ` + mcpServerCols + ` FROM mcp_server
		WHERE tenant_id = $1 AND ($2::text IS NULL OR name ILIKE $2)`
	tail, tailArgs, err := keysetTail("mcp_server", mcpServerKeys, desc, cursor, limit, 2)
	if err != nil {
		return "", nil, err
	}
	return base + tail, append([]any{tenantID, likeSearchArg(search)}, tailArgs...), nil
}

// queryMCPServers runs sql (with args) and scans every row into an MCPServer
// via scanMCPServer, so the secret_ref NULL-handling stays in one place.
func (s *Store) queryMCPServers(ctx context.Context, sql string, args ...any) ([]MCPServer, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list mcp_server: %w", err)
	}
	defer rows.Close()
	var out []MCPServer
	for rows.Next() {
		m, err := scanMCPServer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan mcp_server: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListMCPServersByTenant returns ALL catalog entries for a tenant, name-ordered.
// It is the unpaginated convenience over ListMCPServersPage — the gateway's
// session build, /v1/catalog and the slug-collision check all need the full
// set, unfiltered: none of those callers is a search box, so search is
// always "".
func (s *Store) ListMCPServersByTenant(ctx context.Context, tenantID string) ([]MCPServer, error) {
	return s.ListMCPServersPage(ctx, tenantID, nil, 0, false, "")
}

// ListMCPServersPage returns up to limit catalog entries for a tenant ordered
// (name, id), or (name DESC, id DESC) when desc is true (?order=desc) --
// starting strictly after cursor. limit <= 0 means no limit. search is an
// optional ?q= substring match against name, case-insensitive and unindexed
// by design, see likeSearchArg's doc comment (paging.go). "" means no filter.
func (s *Store) ListMCPServersPage(ctx context.Context, tenantID string, cursor *ListCursor, limit int, desc bool, search string) ([]MCPServer, error) {
	sql, args, err := mcpServerPageSQL(tenantID, cursor, limit, desc, search)
	if err != nil {
		// Distinct prefix from queryMCPServers' query-failure branch: this is
		// a cursor-shape/arity error building the SQL, not a DB error running
		// it. It reaches HTTP already validated per-shape (Task 6), so a
		// caller hitting this is a programming error — 500 is the right
		// answer.
		return nil, fmt.Errorf("mcp_server page cursor: %w", err)
	}
	return s.queryMCPServers(ctx, sql, args...)
}

// UpdateMCPServer updates a catalog entry (scoped to its tenant), enforcing
// optimistic concurrency via m.RowVersion (the version the caller last read),
// and returns the fresh row. This function must defend against a malformed id
// itself — it cannot assume a caller has already validated it: the store API
// is called directly by tests, and handleUpdateServer's own precondition read
// (admin_servers.go, a Task-3 stopgap slated for replacement by Task 6's real
// If-Match handling) is not a guarantee this function can rely on. A single
// statement must also distinguish "doesn't exist" (ErrNotFound) from "exists
// but the version is stale" (ErrVersionMismatch): a plain UPDATE...RETURNING
// cannot tell those apart, since both return zero rows.
//
// secretRef and tlsCARef are the tri-state PUT contract for the two opaque
// reference columns (audit B37/defect 1 of the 2026-09-01 fix): nil means
// "this write does not mention the field, leave the stored value exactly as
// it is"; a pointer to "" means an explicit clear (stored as NULL, matching
// CreateMCPServer's own NULLIF convention); a pointer to a non-empty string
// replaces it. m.SecretRef/m.TLSCARef are IGNORED by this function — the two
// explicit parameters are the only inputs that reach the SET clause — so a
// caller cannot accidentally revive the old bug by populating those fields on
// m instead of passing the tri-state params: CreateMCPServer is the only
// function that still reads them, and it has no "existing value" to preserve
// in the first place, so its plain-string NULLIF-of-empty-string contract is
// unaffected and deliberately unchanged (a create's absent field and an
// explicit empty string mean the same thing: no reference at all).
//
// The CASE expressions below are why no precedent fetch is needed here even
// though "leave unchanged" now requires knowing the current value: the
// decision (keep the column / NULL it / replace it) is made IN SQL, against
// the row this statement already holds, in the same statement that enforces
// the row_version guard — so the "leave unchanged" outcome is exactly as
// race-safe as every other field this UPDATE writes, rather than resting on
// a caller-side read-then-merge that a concurrent write could invalidate.
//
// The CTE below runs the UPDATE exactly once — a data-modifying CTE always
// does, regardless of how many times its name is referenced by the outer
// query — and reports existence and update-success as two counts. On success
// the fresh row is re-read via GetMCPServer rather than folded into the same
// statement's RETURNING: that would need a LEFT JOIN against a dummy row (to
// guarantee exactly one output row even when the UPDATE matches zero) with a
// fully nullable-scan duplicate of scanMCPServer, which is meaningfully more
// code for a codepath that is not hot (admin console traffic).
func (s *Store) UpdateMCPServer(ctx context.Context, m MCPServer, secretRef, tlsCARef *string) (MCPServer, error) {
	const q = `
		WITH cur AS (SELECT 1 FROM mcp_server WHERE tenant_id=$1 AND id=$2),
		     upd AS (
		       UPDATE mcp_server SET
		         name=$3, description=$4, transport=$5, endpoint_or_command=$6,
		         version=$7, protocol_version=$8,
		         secret_ref = CASE WHEN $9::text IS NULL THEN secret_ref ELSE NULLIF($9,'') END,
		         tls_ca_ref = CASE WHEN $10::text IS NULL THEN tls_ca_ref ELSE NULLIF($10,'') END,
		         status=$11
		       WHERE tenant_id=$1 AND id=$2 AND row_version=$12
		       RETURNING 1
		     )
		SELECT (SELECT count(*) FROM cur), (SELECT count(*) FROM upd)`
	var existsCnt, updCnt int
	err := s.db.QueryRow(ctx, q,
		m.TenantID, m.ID, m.Name, m.Description, m.Transport, m.EndpointOrCommand,
		m.Version, m.ProtocolVersion, secretRef, tlsCARef, m.Status, m.RowVersion,
	).Scan(&existsCnt, &updCnt)
	if err != nil {
		// idCastNotFound's pgx.ErrNoRows arm never fires here (the SELECT above
		// is two scalar subqueries with no FROM clause, so it always returns
		// exactly one row); its 22P02 arm is what actually matters — id=$2 still
		// undergoes the uuid cast in both CTEs' WHERE clauses, so a malformed id
		// still surfaces here exactly as it did before this rewrite. Preserving
		// this mapping is deliberate: dropping it regresses malformed-id
		// 404 -> 500 (the v1.16.0 defect class).
		if idCastNotFound(err) {
			return MCPServer{}, ErrNotFound
		}
		return MCPServer{}, fmt.Errorf("update mcp_server: %w", err)
	}
	if existsCnt == 0 {
		return MCPServer{}, ErrNotFound
	}
	if updCnt == 0 {
		return MCPServer{}, ErrVersionMismatch
	}
	updated, err := s.GetMCPServer(ctx, m.TenantID, m.ID)
	if err != nil {
		return MCPServer{}, fmt.Errorf("update mcp_server: reread: %w", err)
	}
	return updated, nil
}

// DeleteMCPServer removes a catalog entry scoped to its tenant. Unlike the
// artifact handlers, handleDeleteServer does not precede this call with a
// separate fetch, so a malformed id must be handled HERE.
func (s *Store) DeleteMCPServer(ctx context.Context, tenantID, id string) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM mcp_server WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		if idCastNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("delete mcp_server: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
