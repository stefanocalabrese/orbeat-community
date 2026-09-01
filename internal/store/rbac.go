package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Role is a named grouping mapped from the IdP's roles/groups.
type Role struct {
	ID       string
	TenantID string
	Name     string
	// RowVersion is the optimistic-concurrency token (migration 00027),
	// maintained by a BEFORE UPDATE trigger and never by a statement.
	RowVersion int64
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
	// RowVersion is the optimistic-concurrency token (migration 00026),
	// maintained by a BEFORE UPDATE trigger and never by a statement. Zero on
	// values that did not come from a read: CreateEntitlement does not project
	// it, and no caller needs it there.
	RowVersion int64
}

// CreateRole inserts a role and returns it.
func (s *Store) CreateRole(ctx context.Context, tenantID, name string) (Role, error) {
	r := Role{TenantID: tenantID, Name: name}
	err := s.db.QueryRow(ctx,
		`INSERT INTO role (tenant_id, name) VALUES ($1,$2) RETURNING id::text, row_version`,
		tenantID, name,
	).Scan(&r.ID, &r.RowVersion)
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
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.RowVersion); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// roleKeys is role's sort order (id appended by keysetTail).
var roleKeys = []sortKey{{Col: "name", Cast: "text"}}

// RoleCursor is the keyset position just after r, walked in the direction
// desc (docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 3's ?order):
// it must match whatever direction produced r, or the cursor's Sort identity
// will not match a same-direction replay either (sortIdentity's doc comment).
func RoleCursor(r Role, desc bool) ListCursor {
	return ListCursor{Keys: []string{r.Name}, ID: r.ID, Sort: sortIdentity("role", roleKeys, desc)}
}

// rolePageSQL builds the tenant-scoped keyset page query and returns the
// COMPLETE bind-ordered arg list. Split out from ListRolesPage so the
// index-usage test can EXPLAIN the exact SQL that runs in production — a test
// that rebuilt the query itself could drift from it and would then prove nothing.
//
// $2 is the ?q= substring search filter (docs/plans/orbeat-admin-search-sort-
// 2026-08-27.md Task 4): NULL (no filter, likeSearchArg's zero case) or an
// ILIKE pattern matched against name, role's own sort column, so search and
// sort agree on which text a user is scanning. Applied in the WHERE clause
// BEFORE keysetTail's cursor predicate and LIMIT, never as a Go filter over
// the returned page: v1.22.0 shipped exactly that bug for ?state (a filter
// applied after LIMIT silently drops rows a page should have carried), and
// TestListRolesSearchComposesWithPaging (paging_test.go) pins this the same
// way TestArtifactListStateFilterFullPage pinned that fix.
func rolePageSQL(tenantID string, cursor *ListCursor, limit int, desc bool, search string) (string, []any, error) {
	const base = `SELECT id::text, tenant_id::text, name, row_version FROM role
		WHERE tenant_id = $1 AND ($2::text IS NULL OR name ILIKE $2)`
	tail, tailArgs, err := keysetTail("role", roleKeys, desc, cursor, limit, 2)
	if err != nil {
		return "", nil, err
	}
	return base + tail, append([]any{tenantID, likeSearchArg(search)}, tailArgs...), nil
}

// ListRolesPage returns up to limit roles for a tenant ordered (name, id) --
// or (name DESC, id DESC) when desc is true (?order=desc), starting strictly
// after cursor. limit <= 0 means no limit. search is an optional ?q=
// substring match against name, case-insensitive and unindexed by design,
// see likeSearchArg's doc comment (paging.go) for both calls. "" means no
// filter.
func (s *Store) ListRolesPage(ctx context.Context, tenantID string, cursor *ListCursor, limit int, desc bool, search string) ([]Role, error) {
	sql, args, err := rolePageSQL(tenantID, cursor, limit, desc, search)
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
		`SELECT id::text, tenant_id::text, name, row_version FROM role WHERE tenant_id = $1 AND name = ANY($2)`,
		tenantID, names)
}

// GetRole returns one role scoped to its tenant, including the row_version an
// If-Match update needs. Mirrors GetArtifact/GetMCPServer/GetEntitlement.
func (s *Store) GetRole(ctx context.Context, tenantID, id string) (Role, error) {
	var r Role
	err := s.db.QueryRow(ctx,
		`SELECT id::text, tenant_id::text, name, row_version FROM role WHERE tenant_id=$1 AND id=$2`,
		tenantID, id).Scan(&r.ID, &r.TenantID, &r.Name, &r.RowVersion)
	if err != nil {
		if idCastNotFound(err) {
			return Role{}, ErrNotFound
		}
		return Role{}, fmt.Errorf("get role: %w", err)
	}
	return r, nil
}

// roleNameUniqueConstraint is the name Postgres auto-generated for
// UNIQUE (tenant_id, name) on role (00001_init.sql declares it with no
// explicit CONSTRAINT name, so Postgres falls back to its
// "<table>_<col1>_<col2>_key" convention) — confirmed against a real 23505
// raised by TestUpdateRoleNameCollision rather than assumed from the naming
// convention alone. Unexported: unlike artifact.go's ApprovedIdentityUnique-
// Index, nothing outside this file ever needs to name it, since UpdateRoleName
// is the only writer that can hit it and it maps the 23505 to ErrNameTaken
// itself.
const roleNameUniqueConstraint = "role_tenant_id_name_key"

// ErrNameTaken reports that a role rename collided with UNIQUE (tenant_id,
// name): another role in the same tenant already holds the requested name.
// The API layer maps it to 409, mirroring how ErrNotFound/ErrVersionMismatch
// map to 404/412.
var ErrNameTaken = errors.New("store: name already taken")

// UpdateRoleName renames a role, refusing a stale write. expected is the
// row_version the caller last read.
//
// Mirrors UpdateEntitlement/UpdateMCPServer/UpdateArtifact's CTE shape
// exactly: two counts distinguish "no such row" (ErrNotFound -> 404) from
// "exists but stale" (ErrVersionMismatch -> 412), which a plain
// UPDATE...RETURNING cannot tell apart since both return zero rows.
//
// A 23505 against roleNameUniqueConstraint surfaces as ErrNameTaken rather
// than a raw *pgconn.PgError, following artifact.go's
// asApprovedIdentityConflict discipline of turning a specific named
// constraint's violation into something the API layer can errors.Is against
// instead of inspecting Postgres error internals itself. Renaming a role onto
// its OWN current name is not a collision: Postgres' uniqueness check never
// compares a row against itself, only against every other row, so that write
// succeeds (and still bumps row_version, like any other update).
func (s *Store) UpdateRoleName(ctx context.Context, tenantID, id, newName string, expected int64) (Role, error) {
	const q = `
		WITH cur AS (SELECT 1 FROM role WHERE tenant_id=$1 AND id=$2),
		     upd AS (
		       UPDATE role SET name=$3
		       WHERE tenant_id=$1 AND id=$2 AND row_version=$4
		       RETURNING 1
		     )
		SELECT (SELECT count(*) FROM cur), (SELECT count(*) FROM upd)`
	var existsCnt, updCnt int
	err := s.db.QueryRow(ctx, q, tenantID, id, newName, expected).Scan(&existsCnt, &updCnt)
	if err != nil {
		// Same reasoning as UpdateMCPServer/UpdateEntitlement: the SELECT
		// above is two scalar subqueries with no FROM clause, so it always
		// returns exactly one row (idCastNotFound's pgx.ErrNoRows arm never
		// fires here); its 22P02 arm is what matters, since id=$2 still
		// undergoes the uuid cast in both CTEs' WHERE clauses.
		if idCastNotFound(err) {
			return Role{}, ErrNotFound
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == roleNameUniqueConstraint {
			return Role{}, ErrNameTaken
		}
		return Role{}, fmt.Errorf("update role name: %w", err)
	}
	if existsCnt == 0 {
		return Role{}, ErrNotFound
	}
	if updCnt == 0 {
		return Role{}, ErrVersionMismatch
	}
	return s.GetRole(ctx, tenantID, id)
}

// entitlementKeys is entitlement's sort order (id is appended by keysetTail).
// Defined next to the query so the cursor constructor below cannot drift from
// the ORDER BY it has to match.
//
// entitlement carries NO ?q= search filter, and that is Task 4's Decision 1
// (docs/plans/orbeat-admin-search-sort-2026-08-27.md), not an oversight: its
// only sort key, and its only natural identity, is role_id, a uuid with no
// text a substring match could ever compare against. The alternative was
// joining to role.name so a search box would have SOMETHING to match, but
// that drags a JOIN into a keyset query that today has none (every other
// list here is a single-table SELECT), for a column that isn't even part of
// entitlementKeys, so the "resume at this keyset position" cursor semantics
// would need re-deriving against a joined result set rather than extending
// the existing single-table reasoning. Refusing is louder and smaller: the
// API layer (admin_entitlements.go's refuseSearch) rejects ?q= on this route
// with 400 rather than silently accepting and ignoring it: a search box
// that appears to filter and does not is worse than one that says it cannot.
// Revisit only alongside a real join, in its own reviewed migration, not as
// a client-side request.
var entitlementKeys = []sortKey{{Col: "role_id", Cast: "uuid"}}

// EntitlementCursor is the keyset position just after e, walked in direction
// desc (?order), see RoleCursor's doc comment for why desc must match.
func EntitlementCursor(e Entitlement, desc bool) ListCursor {
	return ListCursor{Keys: []string{e.RoleID}, ID: e.ID, Sort: sortIdentity("entitlement", entitlementKeys, desc)}
}

// entitlementPageSQL builds the tenant-scoped keyset page query and its
// COMPLETE argument list (tenantID included, in bind order). Split out from
// ListEntitlementsPage so the index-usage test can EXPLAIN the exact
// SQL+args pair that runs in production, and so the caller never has to
// reconstruct the correspondence between the two by hand.
func entitlementPageSQL(tenantID string, cursor *ListCursor, limit int, desc bool) (string, []any, error) {
	const base = `
		SELECT id::text, tenant_id::text, role_id::text, mcp_server_id::text, allowed_tools, permissions, row_version
		FROM entitlement WHERE tenant_id = $1`
	tail, tailArgs, err := keysetTail("entitlement", entitlementKeys, desc, cursor, limit, 1)
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
		if err := rows.Scan(&e.ID, &e.TenantID, &e.RoleID, &e.MCPServerID, &e.AllowedTools, &e.Permissions, &e.RowVersion); err != nil {
			return nil, fmt.Errorf("scan entitlement: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEntitlementsPage returns up to limit entitlements for a tenant ordered
// (role_id, id), or (role_id DESC, id DESC) when desc is true (?order=desc) --
// starting strictly after cursor. limit <= 0 means no limit.
func (s *Store) ListEntitlementsPage(ctx context.Context, tenantID string, cursor *ListCursor, limit int, desc bool) ([]Entitlement, error) {
	sql, args, err := entitlementPageSQL(tenantID, cursor, limit, desc)
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
// GetEntitlement returns one entitlement scoped to its tenant, including the
// row_version an If-Match update needs.
func (s *Store) GetEntitlement(ctx context.Context, tenantID, id string) (Entitlement, error) {
	var e Entitlement
	err := s.db.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, role_id::text, mcp_server_id::text,
		       allowed_tools, permissions, row_version
		FROM entitlement WHERE tenant_id=$1 AND id=$2`, tenantID, id).
		Scan(&e.ID, &e.TenantID, &e.RoleID, &e.MCPServerID, &e.AllowedTools, &e.Permissions, &e.RowVersion)
	if err != nil {
		if idCastNotFound(err) {
			return Entitlement{}, ErrNotFound
		}
		return Entitlement{}, fmt.Errorf("get entitlement: %w", err)
	}
	return e, nil
}

// UpdateEntitlement full-replaces allowed_tools and permissions on one
// entitlement, refusing a stale write.
//
// role_id and mcp_server_id are deliberately NOT updatable. Repointing an
// entitlement at a different role or server is not an edit of this grant, it is
// the revocation of one grant and the creation of another, and collapsing those
// into a PUT would let a single request move access between principals while
// the audit trail records an "update". Delete and create remain the way to do
// that, and they leave two events that say what happened.
//
// Mirrors UpdateMCPServer's CTE shape rather than inventing a second one: the
// two counts distinguish "no such row" (404) from "stale version" (412), which
// a plain UPDATE's RowsAffected cannot.
func (s *Store) UpdateEntitlement(ctx context.Context, e Entitlement) (Entitlement, error) {
	if e.Permissions == nil {
		e.Permissions = []string{}
	}
	const q = `
		WITH cur AS (SELECT 1 FROM entitlement WHERE tenant_id=$1 AND id=$2),
		     upd AS (
		       UPDATE entitlement SET allowed_tools=$3, permissions=$4
		       WHERE tenant_id=$1 AND id=$2 AND row_version=$5
		       RETURNING 1
		     )
		SELECT (SELECT count(*) FROM cur), (SELECT count(*) FROM upd)`
	var existsCnt, updCnt int
	err := s.db.QueryRow(ctx, q, e.TenantID, e.ID, e.AllowedTools, e.Permissions, e.RowVersion).
		Scan(&existsCnt, &updCnt)
	if err != nil {
		// Same reasoning as UpdateMCPServer: the pgx.ErrNoRows arm cannot fire
		// (two scalar subqueries always return one row), but the 22P02 arm is
		// what keeps a malformed id a 404 rather than the 500 of the v1.16.0
		// defect class.
		if idCastNotFound(err) {
			return Entitlement{}, ErrNotFound
		}
		return Entitlement{}, fmt.Errorf("update entitlement: %w", err)
	}
	if existsCnt == 0 {
		return Entitlement{}, ErrNotFound
	}
	if updCnt == 0 {
		return Entitlement{}, ErrVersionMismatch
	}
	return s.GetEntitlement(ctx, e.TenantID, e.ID)
}

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
		       allowed_tools, permissions, row_version
		FROM entitlement
		WHERE tenant_id = $1 AND role_id = ANY($2)`,
		tenantID, roleIDs,
	)
}

// MaxGrantNames caps the name lists that describe rows the caller can still
// go and read. It does NOT bound the work done. Counts stay exact, and the
// underlying reads and the DELETE itself carry no SQL LIMIT: deleting a role
// granted 5 000 artifacts still reads and cascades all 5 000 rows. What this
// constant bounds is the audit_event.metadata jsonb blob: without it, that
// same deletion would write 5 000 names into a single audit row. It bounds
// one API response field too, since ArtifactRoleGrants
// (artifact_entitlement.go) reports through the same helper and the same cap.
//
// IT DOES NOT APPLY TO EVERY LIST, and it did until 2026-08-30. Three lists
// pass through readGrantNames and only two of them name rows that survive:
// an mcp_server and an artifact are still there after the role that was
// granted them is deleted, and their names are one admin-list call away, so
// a capped list costs an operator a second request and nothing else.
// RevokedGrants.VirtualKeyClientIDs is the exception, and it is not a
// borderline one: those rows are DESTROYED by the cascade, the list is the
// only place their client_ids are ever written down, and a role with 60 keys
// permanently lost 10 Keycloak client ids the moment the audit row was
// written. That list is now read with `uncapped` (DeleteRole below). The
// bound it gives up is worth what it buys: a client_id is roughly 40 bytes,
// so even 1 000 keys is about 50 KB of jsonb, written once, by a DELETE
// nobody runs in a loop.
//
// Exported (unlike the rest of this file's unexported helpers) because
// internal/api's sync.list audit metadata caps its own overridden-pin name
// list against this exact number rather than a second, silently-divergent
// literal 50 of its own. That caller builds its list in memory from a
// resolution it already computed and never calls readGrantNames, so it needs
// the number directly; ApprovedIdentityUniqueIndex (artifact.go) is the same
// kind of export for the same reason, a cross-package caller needing the one
// number this package already owns.
const MaxGrantNames = 50

// uncapped is readGrantNames' maxNames argument for a list that must be
// returned whole. It is a named constant rather than a literal 0 at the call
// site because "0" at the end of a five-argument call reads as an offset, a
// limit of none, or a forgotten field, and this one is a deliberate decision
// with a written reason (see MaxGrantNames above and DeleteRole's virtual-key
// read below).
const uncapped = 0

// RevokedGrants reports what deleting a role cascaded away. Counts are always
// exact; ServerNames and ArtifactNames are capped at MaxGrantNames each and
// Truncated says whether that cap bit on either, while
// VirtualKeyClientIDs is returned WHOLE however long it is (see
// MaxGrantNames for why that one list is exempt, and Truncated's own comment
// below for what the flag does and does not cover as a result).
// ServerNames/ArtifactNames/
// VirtualKeyClientIDs are never nil: a zero-grant role reports empty
// (non-nil) slices, so a caller (Task 2's audit metadata) never has to
// special-case JSON `null` vs `[]`. RoleName is the deleted role's name,
// captured by the same locking SELECT that establishes existence/tenancy
// (see DeleteRole), and a caller building an audit record needs the name of
// what it just deleted: this is that value at zero extra round-trips.
//
// IT DESCRIBED TWO OF FIVE CHILDREN FOR TWO RELEASES (A10, fixed
// 2026-08-30). role carries seven inbound foreign keys across five child
// tables, every one ON DELETE CASCADE. Migration 00020 added virtual_key and
// 00022 added usage_daily and role_quota, each with the whole suite green,
// because the test named for this exact property
// (TestDeleteRoleReportsWhatTheCascadeRevoked) seeds only the two children
// that were already reported. Reproduced on real Postgres 18: one DELETE
// destroyed 25 virtual keys, 1 quota row, 500 usage rows and 200 artifact
// entitlements and counted the last of those alone. The set of children is
// now pinned by TestInboundForeignKeysOnParentTables (cascade_index_test.go),
// so a sixth one fails a gate instead of waiting for an audit.
//
// EACH CHILD IS REPORTED IN THE SHAPE AN OPERATOR CAN ACT ON, which is not
// the same shape for all of them:
//
//   - VirtualKeyClientIDs carries client_id, deliberately NOT the key's
//     name. client_id is unique per tenant (virtual_key_client_id_uniq,
//     00020) and name is not; it is the id of the Keycloak client this
//     DELETE just orphaned, and the exact argument
//     bestEffortDeleteKeycloakClient hands dcrDelete
//     (internal/api/admin_virtual_keys.ee.go), so it is the string a realm
//     admin types to finish the cleanup by hand; and it is what the
//     virtualkey.create and virtualkey.revoke audit records already carry as
//     clientId, so this list joins to the rest of the trail. A name does
//     none of that.
//
//     IT IS NOT WHAT IDENTIFIES THE ROBOT'S TRAFFIC, and this comment said
//     it was until 2026-08-30. usage_daily.subject and the gateway's request
//     log both carry p.Subject, the token's sub claim, for a robot exactly
//     as for a human: internal/gateway/server.go builds every session with
//     `subject: p.Subject` and nothing else ever writes that field, so
//     rbac_middleware.go's usage.Count and server.go's recordSubject both
//     receive it. client_id is p.ClientID, which internal/auth/principal.go
//     reads from a different claim, azp, whose own comment records that
//     there is no client_id claim at all. So an operator who takes an id off
//     this list and filters usage_daily.subject or a log line by it gets
//     nothing back, and the metering that key's calls produced is described
//     by UsageRows/UsageCalls below rather than by anything in this list.
//
//     This list is still the ONLY surviving handle on those clients: the row
//     is gone rather than revoked, so the orphan query virtual_key.ee.go
//     points operators at (GET /v1/admin/virtual-keys?revoked=true) cannot
//     return them. Cleaning Keycloak up is still manual, and out of scope
//     here. That is exactly why this one list is read `uncapped` while its
//     two siblings are not: a name this list drops is not recoverable from
//     anywhere, by anyone, ever.
//
//   - QuotaMonthlyCalls is a number, not a list: role_quota is UNIQUE
//     (tenant_id, role_id) (00022), so there is at most one row and it has
//     no name. The number is what an operator re-creating the role needs.
//     Nil means the role had no quota, which is a different fact from a
//     destroyed quota of zero.
//
//   - usage_daily gets counts only, no list. Its rows are keyed on (day,
//     subject, server, tool), so fifty of them name nothing an operator
//     recognises and would be a fair-sized blob of noise in one audit row.
//     UsageRows says how many metering buckets went; UsageCalls says how
//     many attributed calls they held, which is the number that matters
//     when someone later reconciles a usage report against a quota.
//
// RevokedVirtualKey is one destroyed virtual key's Keycloak cleanup data.
//
// Two plain strings rather than a store.VirtualKey: this file is SHARED (not
// *.ee.go), and naming an Enterprise-only type here would put it into a file
// TestNoSharedFileReferencesEnterpriseSymbol scans. Both columns exist in
// every edition's schema, because migrations are not edition-split.
type RevokedVirtualKey struct {
	ClientID string
	// RegistrationAccessTokenSealed is a CREDENTIAL. See
	// RevokedGrants.VirtualKeysForCleanup for where it may and may not go.
	RegistrationAccessTokenSealed string
}

type RevokedGrants struct {
	RoleName             string
	Entitlements         int
	ArtifactEntitlements int
	ServerNames          []string
	ArtifactNames        []string
	// VirtualKeys counts the robot credentials this role capped, and
	// VirtualKeyClientIDs names them. Every one of those credentials stops
	// working on its next tools/call, and its Keycloak client outlives it.
	VirtualKeys         int
	VirtualKeyClientIDs []string
	// VirtualKeysForCleanup carries what deleting each destroyed key's
	// Keycloak client needs, and is DELIBERATELY SEPARATE from
	// VirtualKeyClientIDs above rather than an enrichment of it.
	//
	// RegistrationAccessTokenSealed is a credential. It must never be written
	// to an audit row, a log line or an API response, and the only reason this
	// is safe to carry on a struct whose other fields are all audit metadata
	// is that it lives in its own field with its own name: an edit that adds
	// "every field of RevokedGrants" to a metadata map has to name this one to
	// leak it. Its single consumer is handleDeleteRole's post-commit Keycloak
	// cleanup.
	VirtualKeysForCleanup []RevokedVirtualKey
	// UsageRows and UsageCalls describe the metering history destroyed: how
	// many (day, subject, server, tool) buckets, and how many attributed
	// calls they held between them.
	UsageRows  int
	UsageCalls int64
	// QuotaMonthlyCalls is the destroyed role_quota row's cap, or nil when
	// the role carried no quota.
	QuotaMonthlyCalls *int64
	// Truncated covers ServerNames and ArtifactNames AND NOTHING ELSE. It
	// meant "one of the three lists capped" until 2026-08-30, when the
	// virtual-key list stopped being capped at all; leaving the flag
	// described as covering every list would have made a `truncated: false`
	// read as a claim about a list the flag no longer watches.
	//
	// There is deliberately no second flag for the virtual-key list, because
	// there is nothing for one to say: that list is always complete, so a
	// flag would be a constant false. The question it would have answered is
	// already answerable from the two fields beside it -- VirtualKeys is the
	// exact count and len(VirtualKeyClientIDs) is what was reported, so
	// VirtualKeys > len(VirtualKeyClientIDs) is the whole test, and it is now
	// an invariant violation rather than an expected state
	// (TestDeleteRoleVirtualKeyListIsNeverCapped).
	Truncated bool
}

// readGrantNames runs a single-column name-listing query, bound to (tenantID,
// id), and returns up to maxNames names, the exact count, and whether that cap
// bit. `id` is whichever side of a grant the caller is looking up FROM: a
// role id for DeleteRole's three calls below (the server names, artifact names
// and virtual-key client_ids attached TO that role), an artifact id for
// ArtifactRoleGrants (artifact_entitlement.go: the role names a single
// artifact is granted to).
//
// maxNames is per CALL SITE and not a package-wide constant, because the
// three DeleteRole call sites do not want the same answer: two pass
// MaxGrantNames and one passes `uncapped`. Making it an argument is what
// keeps that a visible decision at each site instead of a property of this
// helper that a fourth caller inherits without noticing.
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
func (s *Store) readGrantNames(ctx context.Context, sql, tenantID, id string, maxNames int) (names []string, n int, truncated bool, err error) {
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
		// maxNames <= 0 (the `uncapped` constant) keeps every row and can
		// never set truncated. Written as a guard on maxNames rather than as
		// a separate uncapped loop so that the scan, the exact count and the
		// idCastNotFound ordering below stay in ONE place, which is the whole
		// reason this helper was factored out.
		if maxNames <= 0 || len(names) < maxNames {
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
// role is referenced ON DELETE CASCADE by SEVEN foreign keys across FIVE
// child tables: entitlement and artifact_entitlement by two FK paths each
// (the originals in 00001/00004 plus the composite-tenant FKs added, not
// replaced, by 00010), then virtual_key (00020) and usage_daily and
// role_quota (00022). So this one DELETE silently revokes every server and
// artifact grant hung off the role, kills every robot credential capped by
// it, drops its monthly quota, and erases its metering history.
//
// Every one of those five is counted below and returned in RevokedGrants,
// which is a correction: it reported two of them until A10 (2026-08-30).
// See RevokedGrants for why each child is reported in the shape it is, and
// TestInboundForeignKeysOnParentTables (cascade_index_test.go) for the gate
// that fails when a sixth child appears.
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
// once unblocked, the reads below run in fresh per-statement snapshots
// (READ COMMITTED) that already include whatever the holder committed.
//
// THAT LOCK COVERS ALL FIVE CHILDREN, AND NOT BY APP-LEVEL CONVENTION.
// handleCreateVirtualKey takes RoleExistsInTenant's FOR SHARE lock exactly
// as handleCreateEntitlement does, and the quota handler takes FOR UPDATE
// via LockRoleForQuotaWrite, so both serialize against this statement the
// same way. usage_daily has no such caller at all: IncrementUsage is flushed
// from a background counter that never touches the role row. What holds
// there is Postgres itself, because inserting a child row makes the
// referential-integrity check take FOR KEY SHARE on the parent, and FOR KEY
// SHARE conflicts with FOR UPDATE. That is a claim about the engine rather
// than about this codebase, so it is measured rather than assumed:
// TestRoleForUpdateBlocksUnlockedChildInsert holds this exact statement open
// and watches a usage_daily insert block on it.
//
// So what is reported is exactly what the following DELETE destroys, not a
// racy before-picture. That sentence was TRUE WHEN WRITTEN AND FALSE FROM
// MIGRATION 00020 ONWARDS, for a reason the lock had nothing to do with:
// three of the five children were never read at all. A rationale about
// concurrency stayed correct on its own terms while the thing it was
// asserting had stopped being true.
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
		ORDER BY m.name`, tenantID, id, MaxGrantNames)
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
		ORDER BY a.name`, tenantID, id, MaxGrantNames)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RevokedGrants{}, ErrNotFound
		}
		return RevokedGrants{}, fmt.Errorf("delete role: read artifact grants: %w", err)
	}

	// client_id, not name: see RevokedGrants for why this column and no
	// other.
	//
	// `uncapped`, unlike the two reads above, and the third return value is
	// discarded because it is now always false. The two lists above name rows
	// the DELETE leaves alone, so a cap costs an operator one more admin-list
	// call; this one names rows the DELETE destroys, so a cap costs those
	// client_ids permanently -- a role with 60 keys used to write 50 into the
	// audit record and lose the other 10 at that instant, with no query
	// anywhere in orbeat able to name them afterwards. Capo's decision,
	// 2026-08-30: the bound is not worth that, and the size it gives up is
	// small (a client_id is about 40 bytes, so 1 000 keys is roughly 50 KB of
	// jsonb, written once by a DELETE nobody runs in a loop).
	keyClientIDs, keyCount, _, err := s.readGrantNames(ctx, `
		SELECT client_id FROM virtual_key
		WHERE tenant_id = $1 AND role_id = $2
		ORDER BY client_id`, tenantID, id, uncapped)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RevokedGrants{}, ErrNotFound
		}
		return RevokedGrants{}, fmt.Errorf("delete role: read virtual keys: %w", err)
	}

	// A SECOND read of the same rows, for a different consumer, and the two
	// are kept apart on purpose. The list above names the keys for the AUDIT
	// RECORD; this one carries what deleting their Keycloak clients actually
	// needs, and its token field must never reach an audit row, a log line or
	// an API response. Splitting them is what makes that a property of the
	// types rather than of whoever next edits the audit metadata.
	//
	// It must run BEFORE the DELETE below, because the cascade destroys these
	// rows and nothing can recover a registration access token afterwards:
	// Keycloak issues it once, at registration. Until 2026-09-01 nothing read
	// it here at all, so deleting a role orphaned every one of its robots'
	// Keycloak clients while the revoke path (migration 00030) cleaned up
	// correctly. capo's decision: delete them during role deletion, using the
	// mechanism revoke already has.
	cleanupRows, err := s.db.Query(ctx, `
		SELECT client_id, registration_access_token_sealed FROM virtual_key
		WHERE tenant_id = $1 AND role_id = $2
		ORDER BY client_id`, tenantID, id)
	if err != nil {
		return RevokedGrants{}, fmt.Errorf("delete role: read virtual key cleanup data: %w", err)
	}
	var cleanup []RevokedVirtualKey
	for cleanupRows.Next() {
		var k RevokedVirtualKey
		if err := cleanupRows.Scan(&k.ClientID, &k.RegistrationAccessTokenSealed); err != nil {
			cleanupRows.Close()
			return RevokedGrants{}, fmt.Errorf("delete role: scan virtual key cleanup data: %w", err)
		}
		cleanup = append(cleanup, k)
	}
	cleanupRows.Close()
	if err := cleanupRows.Err(); err != nil {
		return RevokedGrants{}, fmt.Errorf("delete role: read virtual key cleanup data: %w", err)
	}

	// One statement for both usage numbers: they come from the same rows and
	// a second round trip could only disagree with the first. COALESCE
	// because sum() over zero rows is NULL, not 0, and a role with no
	// metering history is the common case rather than an edge one.
	var usageRows int
	var usageCalls int64
	if err := s.db.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(calls), 0) FROM usage_daily
		WHERE tenant_id = $1 AND role_id = $2`, tenantID, id,
	).Scan(&usageRows, &usageCalls); err != nil {
		return RevokedGrants{}, fmt.Errorf("delete role: read usage rows: %w", err)
	}

	// pgx.ErrNoRows is the ORDINARY outcome here, not a failure: role_quota
	// is UNIQUE (tenant_id, role_id) and most roles carry no quota at all.
	// It maps to a nil pointer, never to ErrNotFound, which in this function
	// means "no such role" and would be a lie.
	var quota *int64
	if err := s.db.QueryRow(ctx,
		`SELECT monthly_calls FROM role_quota WHERE tenant_id = $1 AND role_id = $2`,
		tenantID, id).Scan(&quota); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return RevokedGrants{}, fmt.Errorf("delete role: read role quota: %w", err)
	}

	tag, err := s.db.Exec(ctx, `DELETE FROM role WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	if err != nil {
		return RevokedGrants{}, fmt.Errorf("delete role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return RevokedGrants{}, ErrNotFound
	}

	return RevokedGrants{
		RoleName:              name,
		Entitlements:          srvCount,
		ArtifactEntitlements:  artCount,
		ServerNames:           srvNames,
		ArtifactNames:         artNames,
		VirtualKeys:           keyCount,
		VirtualKeyClientIDs:   keyClientIDs,
		VirtualKeysForCleanup: cleanup,
		UsageRows:             usageRows,
		UsageCalls:            usageCalls,
		QuotaMonthlyCalls:     quota,
		// Two terms, not three: the virtual-key read above is uncapped and
		// can never contribute one. See Truncated's own comment on
		// RevokedGrants for what the flag now claims.
		Truncated: srvTrunc || artTrunc,
	}, nil
}
