package store

import (
	"context"
	"errors"
	"fmt"
)

// Role is a named grouping mapped from the IdP's roles/groups.
type Role struct {
	ID       string
	TenantID string
	Name     string
}

// Entitlement grants a role access to an MCP server, optionally narrowed to a
// set of tools. AllowedTools == nil means all tools are allowed.
type Entitlement struct {
	ID           string
	TenantID     string
	RoleID       string
	MCPServerID  string
	AllowedTools []string
	Permissions  []string
}

// CreateRole inserts a role and returns it.
func (s *Store) CreateRole(ctx context.Context, tenantID, name string) (Role, error) {
	r := Role{TenantID: tenantID, Name: name}
	err := s.db.QueryRow(ctx,
		`INSERT INTO role (tenant_id, name) VALUES ($1,$2) RETURNING id::text`,
		tenantID, name,
	).Scan(&r.ID)
	if err != nil {
		return Role{}, fmt.Errorf("create role: %w", err)
	}
	return r, nil
}

// CreateEntitlement inserts an entitlement and returns it.
func (s *Store) CreateEntitlement(ctx context.Context, e Entitlement) (Entitlement, error) {
	if e.Permissions == nil {
		e.Permissions = []string{}
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO entitlement
			(tenant_id, role_id, mcp_server_id, allowed_tools, permissions)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id::text`,
		e.TenantID, e.RoleID, e.MCPServerID, e.AllowedTools, e.Permissions,
	).Scan(&e.ID)
	if err != nil {
		return Entitlement{}, fmt.Errorf("create entitlement: %w", err)
	}
	return e, nil
}

// RoleExistsInTenant locks the role row (FOR SHARE) and reports whether it
// belongs to tenantID. The lock prevents the role from being deleted until the
// calling transaction commits, so a caller can safely insert a referencing row.
func (s *Store) RoleExistsInTenant(ctx context.Context, tenantID, id string) (bool, error) {
	var got string
	err := s.db.QueryRow(ctx,
		`SELECT id::text FROM role WHERE tenant_id = $1 AND id = $2 FOR SHARE`,
		tenantID, id).Scan(&got)
	if err != nil {
		// idCastNotFound, not errors.Is(pgx.ErrNoRows): id arrives unvalidated
		// from a JSON body (handleCreateEntitlement), so a malformed value fails
		// the uuid cast rather than matching no row. Both mean "doesn't exist".
		// The only user-supplied cast here is id — tenant_id comes from the authz
		// resolver — so this cannot mask an unrelated cast failure.
		if idCastNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("role exists check: %w", err)
	}
	return true, nil
}

// queryRoles runs sql (with args) and scans every row into a Role. Shared by
// every role list query in this file so the scan logic exists exactly once —
// mirrors artifact.go's queryArtifacts / mcpserver.go's queryMCPServers.
func (s *Store) queryRoles(ctx context.Context, sql string, args ...any) ([]Role, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	defer rows.Close()
	var out []Role
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// roleKeys is role's sort order (id appended by keysetTail).
var roleKeys = []sortKey{{Col: "name", Cast: "text"}}

// RoleCursor is the keyset position just after r.
func RoleCursor(r Role) ListCursor {
	return ListCursor{Keys: []string{r.Name}, ID: r.ID}
}

// rolePageSQL builds the tenant-scoped keyset page query and returns the
// COMPLETE bind-ordered arg list. Split out from ListRolesPage so the
// index-usage test can EXPLAIN the exact SQL that runs in production — a test
// that rebuilt the query itself could drift from it and would then prove nothing.
func rolePageSQL(tenantID string, cursor *ListCursor, limit int) (string, []any, error) {
	const base = `SELECT id::text, tenant_id::text, name FROM role WHERE tenant_id = $1`
	tail, tailArgs, err := keysetTail("role", roleKeys, false, cursor, limit, 1)
	if err != nil {
		return "", nil, err
	}
	return base + tail, append([]any{tenantID}, tailArgs...), nil
}

// ListRolesPage returns up to limit roles for a tenant ordered (name, id),
// starting strictly after cursor. limit <= 0 means no limit.
func (s *Store) ListRolesPage(ctx context.Context, tenantID string, cursor *ListCursor, limit int) ([]Role, error) {
	sql, args, err := rolePageSQL(tenantID, cursor, limit)
	if err != nil {
		// Distinct prefix from queryRoles' query-failure branch: this is a
		// cursor-shape/arity error building the SQL, not a DB error running
		// it. It reaches HTTP already validated per-shape (Task 6), so a
		// caller hitting this is a programming error — 500 is the right
		// answer.
		return nil, fmt.Errorf("role page cursor: %w", err)
	}
	return s.queryRoles(ctx, sql, args...)
}

// GetRolesByNames returns the roles in a tenant matching any of names.
// An empty names slice returns no rows (no error). Unknown names are skipped.
func (s *Store) GetRolesByNames(ctx context.Context, tenantID string, names []string) ([]Role, error) {
	if len(names) == 0 {
		return nil, nil
	}
	return s.queryRoles(ctx,
		`SELECT id::text, tenant_id::text, name FROM role WHERE tenant_id = $1 AND name = ANY($2)`,
		tenantID, names)
}

// entitlementKeys is entitlement's sort order (id is appended by keysetTail).
// Defined next to the query so the cursor constructor below cannot drift from
// the ORDER BY it has to match.
var entitlementKeys = []sortKey{{Col: "role_id", Cast: "uuid"}}

// EntitlementCursor is the keyset position just after e.
func EntitlementCursor(e Entitlement) ListCursor {
	return ListCursor{Keys: []string{e.RoleID}, ID: e.ID}
}

// entitlementPageSQL builds the tenant-scoped keyset page query and its
// COMPLETE argument list (tenantID included, in bind order). Split out from
// ListEntitlementsPage so the index-usage test can EXPLAIN the exact
// SQL+args pair that runs in production, and so the caller never has to
// reconstruct the correspondence between the two by hand.
func entitlementPageSQL(tenantID string, cursor *ListCursor, limit int) (string, []any, error) {
	const base = `
		SELECT id::text, tenant_id::text, role_id::text, mcp_server_id::text, allowed_tools, permissions
		FROM entitlement WHERE tenant_id = $1`
	tail, tailArgs, err := keysetTail("entitlement", entitlementKeys, false, cursor, limit, 1)
	if err != nil {
		return "", nil, err
	}
	return base + tail, append([]any{tenantID}, tailArgs...), nil
}

// queryEntitlements runs sql (with args) and scans every row into an
// Entitlement. Shared by every entitlement list query in this file so the scan
// logic exists exactly once — mirrors artifact.go's queryArtifacts.
func (s *Store) queryEntitlements(ctx context.Context, sql string, args ...any) ([]Entitlement, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list entitlements: %w", err)
	}
	defer rows.Close()
	var out []Entitlement
	for rows.Next() {
		var e Entitlement
		if err := rows.Scan(&e.ID, &e.TenantID, &e.RoleID, &e.MCPServerID, &e.AllowedTools, &e.Permissions); err != nil {
			return nil, fmt.Errorf("scan entitlement: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEntitlementsPage returns up to limit entitlements for a tenant ordered
// (role_id, id), starting strictly after cursor. limit <= 0 means no limit.
func (s *Store) ListEntitlementsPage(ctx context.Context, tenantID string, cursor *ListCursor, limit int) ([]Entitlement, error) {
	sql, args, err := entitlementPageSQL(tenantID, cursor, limit)
	if err != nil {
		// Distinct prefix from queryEntitlements' query-failure branch: this
		// is a cursor-shape/arity error building the SQL, not a DB error
		// running it, and the two must be distinguishable in a log line. It
		// reaches HTTP already validated per-shape (Task 6), so a caller
		// hitting this is a programming error — 500 is the right answer.
		return nil, fmt.Errorf("entitlement page cursor: %w", err)
	}
	return s.queryEntitlements(ctx, sql, args...)
}

// DeleteEntitlement removes an entitlement scoped to its tenant.
func (s *Store) DeleteEntitlement(ctx context.Context, tenantID, id string) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM entitlement WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		if idCastNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("delete entitlement: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListEntitlementsByRoles returns all entitlements in a tenant for any of the
// given role IDs. An empty roleIDs slice returns no rows (no error).
func (s *Store) ListEntitlementsByRoles(ctx context.Context, tenantID string, roleIDs []string) ([]Entitlement, error) {
	if len(roleIDs) == 0 {
		return nil, nil
	}
	return s.queryEntitlements(ctx, `
		SELECT id::text, tenant_id::text, role_id::text, mcp_server_id::text,
		       allowed_tools, permissions
		FROM entitlement
		WHERE tenant_id = $1 AND role_id = ANY($2)`,
		tenantID, roleIDs,
	)
}

// maxGrantNames caps the NAME LISTS a grant report carries. It does NOT
// bound the work done. Counts stay exact, and the underlying reads and the
// DELETE itself carry no SQL LIMIT: deleting a role granted 5 000 artifacts
// still reads and cascades all 5 000 rows. What this constant bounds is the
// audit_event.metadata jsonb blob — without it, that same deletion would
// write 5 000 names into a single audit row. It bounds one API response field
// too, since ArtifactRoleGrants (artifact_entitlement.go) reports through the
// same helper and the same cap.
const maxGrantNames = 50

// RevokedGrants reports what deleting a role cascaded away. Counts are always
// exact; the name lists are capped at maxGrantNames each, and Truncated says
// whether that cap bit. ServerNames/ArtifactNames are never nil — a zero-grant
// role reports empty (non-nil) slices, so a caller (Task 2's audit metadata)
// never has to special-case JSON `null` vs `[]`. RoleName is the deleted
// role's name, captured by the same locking SELECT that establishes
// existence/tenancy (see DeleteRole) — a caller building an audit record
// needs the name of what it just deleted, and this is that value at zero
// extra round-trips.
type RevokedGrants struct {
	RoleName             string
	Entitlements         int
	ArtifactEntitlements int
	ServerNames          []string
	ArtifactNames        []string
	Truncated            bool
}

// readGrantNames runs a single-column name-listing query, bound to (tenantID,
// id), and returns up to maxGrantNames names, the exact count, and whether the
// cap bit. `id` is whichever side of a grant the caller is looking up FROM: a
// role id for DeleteRole's two calls below (the server and artifact names
// granted TO that role), an artifact id for ArtifactRoleGrants
// (artifact_entitlement.go: the role names a single artifact is granted to).
// Factored out of DeleteRole because its call sites are otherwise
// identical except for their SQL and the noun in comments/errors, and the
// part that must stay in sync between them — the Close/rows.Err()/
// idCastNotFound ordering — is exactly the subtle part. Mirrors this file's
// queryRoles/queryEntitlements: "the scan logic exists exactly once".
//
// GOTCHA verified live (not by analogy with the QueryRow-based idCastNotFound
// call sites elsewhere in this file): for a multi-row s.db.Query, pgx v5 does
// NOT surface a uuid-cast bind failure as the error returned by Query itself —
// that call returns a nil error and a valid (empty) Rows. The 22P02 only
// appears on rows.Err() after the Next() loop finds no rows. So the check
// must run on rows.Err(); it is kept on the Query error too as
// belt-and-braces, since that arm is where a non-parameter failure (a
// connection drop, a syntax error) would surface. In DeleteRole specifically
// neither arm can actually observe a 22P02 today: DeleteRole locks (and
// therefore validates) the role row before ever calling this function, so a
// malformed id is already rejected as ErrNotFound before this SQL runs. Both
// idCastNotFound checks here are defense in depth against a future caller of
// this helper that skips that lock, not live not-found detection for the
// current one.
func (s *Store) readGrantNames(ctx context.Context, sql, tenantID, id string) (names []string, n int, truncated bool, err error) {
	names = []string{}
	rows, err := s.db.Query(ctx, sql, tenantID, id)
	if err != nil {
		if idCastNotFound(err) {
			return []string{}, 0, false, ErrNotFound
		}
		return []string{}, 0, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return []string{}, 0, false, err
		}
		n++
		if len(names) < maxGrantNames {
			names = append(names, name)
		} else {
			truncated = true
		}
	}
	if err := rows.Err(); err != nil {
		if idCastNotFound(err) {
			return []string{}, 0, false, ErrNotFound
		}
		return []string{}, 0, false, err
	}
	return names, n, truncated, nil
}

// DeleteRole removes a role and reports the grants its FK cascade revoked.
//
// role is referenced ON DELETE CASCADE from entitlement and artifact_entitlement
// (by two FK paths each — the originals in 00001/00004 plus the composite-tenant
// FKs added, not replaced, by 00010). So this one DELETE silently revokes every
// server and artifact grant hung off the role.
//
// The role row is locked (FOR UPDATE) BEFORE the grant reads run, and this is
// load-bearing, not defensive polish. RoleExistsInTenant takes a FOR SHARE
// lock on the same row and is held open by handleCreateEntitlement /
// handleCreateArtifactEntitlement across their own INSERT. Under this repo's
// READ COMMITTED isolation, reading the grants BEFORE taking a conflicting
// lock lets DeleteRole's reads complete, then block on the final DELETE
// behind that FOR SHARE holder — which can insert ANOTHER grant and commit
// while DeleteRole waits, so the DELETE (once unblocked) cascades N+1 rows
// while the report says N. This was reproduced (not just reasoned about) and
// is pinned by TestDeleteRoleLocksAgainstConcurrentGrantInsert. Taking FOR
// UPDATE first makes DeleteRole block immediately behind any such holder;
// once unblocked, the grant reads below run in fresh per-statement snapshots
// (READ COMMITTED) that already include whatever the holder committed, so
// what is reported is exactly what the following DELETE destroys — not a
// racy before-picture.
//
// The lock also collapses what used to be three separate not-found checks
// (unknown / malformed / cross-tenant id, each re-derived per query) into one
// up-front check: a bad id fails the uuid cast here, on the very first
// statement, before any grant is read.
//
// The lock statement selects name, not just id: it already reads the role row
// under the tenant-scoped WHERE that establishes existence, so scanning the
// name too gets RevokedGrants.RoleName for free — same statement, same lock,
// zero extra round-trips, and no separate GetRoleByID needs to exist. The
// caller (Task 2's handler, building the audit record) would otherwise need
// a second read of the very row this statement already has open.
//
// Returns ErrNotFound for an unknown id, a cross-tenant id, and a malformed one
// (SQLSTATE 22P02 via idCastNotFound) — see v1.16.0.
func (s *Store) DeleteRole(ctx context.Context, tenantID, id string) (RevokedGrants, error) {
	var name string
	err := s.db.QueryRow(ctx,
		`SELECT name FROM role WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenantID, id).Scan(&name)
	if err != nil {
		if idCastNotFound(err) {
			return RevokedGrants{}, ErrNotFound
		}
		return RevokedGrants{}, fmt.Errorf("delete role: lock: %w", err)
	}

	srvNames, srvCount, srvTrunc, err := s.readGrantNames(ctx, `
		SELECT m.name FROM entitlement e
		JOIN mcp_server m ON m.id = e.mcp_server_id
		WHERE e.tenant_id = $1 AND e.role_id = $2
		ORDER BY m.name`, tenantID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RevokedGrants{}, ErrNotFound
		}
		return RevokedGrants{}, fmt.Errorf("delete role: read server grants: %w", err)
	}

	artNames, artCount, artTrunc, err := s.readGrantNames(ctx, `
		SELECT a.name FROM artifact_entitlement ae
		JOIN artifact a ON a.id = ae.artifact_id
		WHERE ae.tenant_id = $1 AND ae.role_id = $2
		ORDER BY a.name`, tenantID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RevokedGrants{}, ErrNotFound
		}
		return RevokedGrants{}, fmt.Errorf("delete role: read artifact grants: %w", err)
	}

	tag, err := s.db.Exec(ctx, `DELETE FROM role WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	if err != nil {
		return RevokedGrants{}, fmt.Errorf("delete role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return RevokedGrants{}, ErrNotFound
	}

	return RevokedGrants{
		RoleName:             name,
		Entitlements:         srvCount,
		ArtifactEntitlements: artCount,
		ServerNames:          srvNames,
		ArtifactNames:        artNames,
		Truncated:            srvTrunc || artTrunc,
	}, nil
}
