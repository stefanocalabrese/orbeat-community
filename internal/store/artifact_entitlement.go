package store

import (
	"context"
	"fmt"
)

// ArtifactEntitlement grants a role access to a role-visibility artifact.
// Mirrors Entitlement (role ↔ mcp_server).
type ArtifactEntitlement struct {
	ID         string
	TenantID   string
	RoleID     string
	ArtifactID string
}

// CreateArtifactEntitlement inserts an entitlement and returns it.
func (s *Store) CreateArtifactEntitlement(ctx context.Context, e ArtifactEntitlement) (ArtifactEntitlement, error) {
	err := s.db.QueryRow(ctx, `
		INSERT INTO artifact_entitlement (tenant_id, role_id, artifact_id)
		VALUES ($1,$2,$3) RETURNING id::text`,
		e.TenantID, e.RoleID, e.ArtifactID).Scan(&e.ID)
	if err != nil {
		return ArtifactEntitlement{}, fmt.Errorf("create artifact entitlement: %w", err)
	}
	return e, nil
}

// DeleteArtifactEntitlement removes an entitlement scoped to its tenant.
func (s *Store) DeleteArtifactEntitlement(ctx context.Context, tenantID, id string) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM artifact_entitlement WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	if err != nil {
		if idCastNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("delete artifact entitlement: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// artifactEntitlementKeys is artifact_entitlement's sort order (id appended by
// keysetTail). Like entitlement, role_id is NOT unique here.
//
// Also like entitlement, this list carries NO ?q= search filter, the same
// Decision 1 (docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 4;
// rbac.go's entitlementKeys carries the full reasoning). role_id is a uuid
// with no text of its own to search, and adding one would mean joining to
// role.name in a keyset query that has no join today. The API layer
// (admin_artifact_entitlements.go's refuseSearch) rejects ?q= on this route
// with 400 instead.
var artifactEntitlementKeys = []sortKey{{Col: "role_id", Cast: "uuid"}}

// ArtifactEntitlementCursor is the keyset position just after e, walked in
// direction desc (?order), see RoleCursor's doc comment (rbac.go) for why
// desc must match.
func ArtifactEntitlementCursor(e ArtifactEntitlement, desc bool) ListCursor {
	return ListCursor{Keys: []string{e.RoleID}, ID: e.ID, Sort: sortIdentity("artifact_entitlement", artifactEntitlementKeys, desc)}
}

// artifactEntitlementPageSQL builds the tenant-scoped keyset page query and
// returns the COMPLETE bind-ordered arg list. Split out from
// ListArtifactEntitlementsPage so the index-usage test can EXPLAIN the exact
// SQL that runs in production — a test that rebuilt the query itself could
// drift from it and would then prove nothing.
//
// This replaces the prior unqualified `ORDER BY role_id` — a C3-class defect
// (see paging.go's keysetTail doc): the projection selects `role_id::text`,
// so the bare name in ORDER BY resolved against that output LABEL, not the
// uuid column, and no index on role_id could serve the sort. Table-qualifying
// via keysetTail fixes it the same way Task 2b fixed audit.go.
func artifactEntitlementPageSQL(tenantID string, cursor *ListCursor, limit int, desc bool) (string, []any, error) {
	const base = `
		SELECT id::text, tenant_id::text, role_id::text, artifact_id::text
		FROM artifact_entitlement WHERE tenant_id=$1`
	tail, tailArgs, err := keysetTail("artifact_entitlement", artifactEntitlementKeys, desc, cursor, limit, 1)
	if err != nil {
		return "", nil, err
	}
	return base + tail, append([]any{tenantID}, tailArgs...), nil
}

// queryArtifactEntitlements runs sql (with args) and scans every row into an
// ArtifactEntitlement. Shared by every artifact-entitlement list query in this
// file so the scan logic exists exactly once — mirrors rbac.go's
// queryEntitlements / artifact.go's queryArtifacts.
func (s *Store) queryArtifactEntitlements(ctx context.Context, sql string, args ...any) ([]ArtifactEntitlement, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list artifact entitlements: %w", err)
	}
	defer rows.Close()
	var out []ArtifactEntitlement
	for rows.Next() {
		var e ArtifactEntitlement
		if err := rows.Scan(&e.ID, &e.TenantID, &e.RoleID, &e.ArtifactID); err != nil {
			return nil, fmt.Errorf("scan artifact entitlement: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListArtifactEntitlementsPage returns up to limit artifact entitlements for a
// tenant ordered (role_id, id), or (role_id DESC, id DESC) when desc is true
// (?order=desc), starting strictly after cursor. limit <= 0 means no limit.
func (s *Store) ListArtifactEntitlementsPage(ctx context.Context, tenantID string, cursor *ListCursor, limit int, desc bool) ([]ArtifactEntitlement, error) {
	sql, args, err := artifactEntitlementPageSQL(tenantID, cursor, limit, desc)
	if err != nil {
		// Distinct prefix from queryArtifactEntitlements' query-failure branch:
		// this is a cursor-shape/arity error building the SQL, not a DB error
		// running it. It reaches HTTP already validated per-shape (Task 6), so
		// a caller hitting this is a programming error — 500 is the right
		// answer.
		return nil, fmt.Errorf("artifact entitlement page cursor: %w", err)
	}
	return s.queryArtifactEntitlements(ctx, sql, args...)
}

// ListEntitledArtifacts returns the artifacts whose APPROVED visibility is
// 'role' and which are entitled to any of roleIDs (the Channel-2 sync read
// path). Empty roleIDs returns no rows.
//
// Projection and filter both read the approved snapshot (migration 00016), so
// this query no longer joins a live name to a frozen body: the name the sync
// client writes to disk and the bytes it writes there were approved together,
// in one transaction, by one reviewer. Before 00016 it took type and name from
// the live row and content from the snapshot, which is why renaming an
// approved artifact had to be refused outright.
//
// approved_visibility rather than visibility means a role -> org flip moves the
// artifact from this channel to the marketplace when the flip is approved, not
// when it is saved. The filter itself is still belt-and-suspenders: only role
// artifacts are ever entitled, but it guarantees an org artifact can never
// escape via sync.
//
// The projection is distArtifactCols verbatim (see its comment in artifact.go:
// the two distribution queries share a positional scan, so a hand-copied list
// that drifts is a runtime scan error rather than a compile error).
//
// The artifact table is NOT aliased to `a` here, and that is what makes the
// shared const work in this query. distArtifactCols qualifies every column
// with `artifact.` because it projects a bare id, and artifact_entitlement has
// an id column of its own: unqualified, that is SQLSTATE 42702, ambiguous
// column reference, in THIS query and legal in the org one. An alias would
// leave the const's `artifact.` prefix unresolvable, so the table name is the
// join's only handle on it. `ae` keeps its alias: nothing in the shared
// projection names that table.
func (s *Store) ListEntitledArtifacts(ctx context.Context, tenantID string, roleIDs []string) ([]Artifact, error) {
	if len(roleIDs) == 0 {
		return nil, nil
	}
	return s.queryDistArtifacts(ctx, `
		SELECT DISTINCT `+distArtifactCols+`
		FROM artifact
		JOIN artifact_entitlement ae ON ae.artifact_id = artifact.id
		WHERE artifact.tenant_id=$1 AND artifact.approved_visibility='role'
		  AND artifact.approved_content IS NOT NULL AND ae.role_id = ANY($2)
		ORDER BY artifact.approved_type, artifact.approved_name`, tenantID, roleIDs)
}

// ArtifactExistsInTenant locks the artifact row (FOR SHARE) and reports whether
// it belongs to tenantID, so a handler can validate and insert a referencing
// entitlement in one transaction without a delete race.
//
// It mirrors RoleExistsInTenant (rbac.go) down to the error mapping, and that
// last part is what this comment used to claim without doing. Both ids reach
// their check unvalidated out of the same JSON body
// (handleCreateArtifactEntitlement), so a malformed value fails Postgres' uuid
// cast with SQLSTATE 22P02 instead of matching no row, and errors.Is(err,
// pgx.ErrNoRows) does not match a 22P02. internal/api/respond.go's fail() has
// no arm for that SQLSTATE either, so a malformed artifactId used to reach the
// client as 500 "internal error" while a malformed roleId on the SAME request
// body returned 400, both measured through the handler. idCastNotFound folds
// the two into "doesn't exist" the way the role sibling always has, and, as
// there, the only user-supplied cast in this statement is id (tenant_id comes
// from the authz resolver), so it cannot mask an unrelated cast failure.
func (s *Store) ArtifactExistsInTenant(ctx context.Context, tenantID, id string) (bool, error) {
	var got string
	err := s.db.QueryRow(ctx,
		`SELECT id::text FROM artifact WHERE tenant_id=$1 AND id=$2 FOR SHARE`,
		tenantID, id).Scan(&got)
	if err != nil {
		if idCastNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("artifact exists check: %w", err)
	}
	return true, nil
}

// ArtifactRoleGrants reports the per-role grants attached to ONE artifact: the
// artifact_entitlement rows pointing at it, resolved to role names.
//
// Those rows are consulted only while the artifact's APPROVED visibility is
// 'role' (ListEntitledArtifacts, above, filters on approved_visibility since
// migration 00016, so the dormancy begins when a flip to 'org' is approved,
// not when it is saved). Flipping an artifact to 'org' does NOT delete them:
// they go DORMANT, and flipping back to 'role' revives every one of them at
// once, with nobody re-granting anything. The retention is deliberate, since
// it is what makes a mistaken flip recoverable, so what this
// type exists for is legibility, not prevention: it is the count and the names
// an admin needs in front of them before such a flip, and in the audit record
// afterwards.
//
// Shaped like RevokedGrants (rbac.go) and capped like its two SURVIVOR lists:
// Count is exact, RoleNames holds at most MaxGrantNames of them, Truncated
// says whether that cap bit, and RoleNames is never nil so a caller never has
// to tell JSON `null` from `[]`. The cap is right here and wrong for
// RevokedGrants.VirtualKeyClientIDs for the same reason: these role rows
// survive the call, so a capped list is one admin-list call away from being
// completed.
type ArtifactRoleGrants struct {
	Count     int
	RoleNames []string
	Truncated bool
}

// artifactRoleGrantsSQL reads one artifact's grant role names, newest schema
// order (r.name, table-qualified: the projection is uncast here, but qualifying
// is what keeps this out of the C3 output-label class documented on
// paging.go's keysetTail).
//
// There is no (tenant_id, artifact_id) index on artifact_entitlement to serve
// this predicate; migration 00012's closing note already records that this
// table's non-role_id access paths were left unindexed as a decision of their
// own. Both callers are admin-only single-artifact routes (GET
// /v1/admin/artifacts/{id} and PUT of the same), so no index rides along with
// this change either. Recorded, not passed over.
const artifactRoleGrantsSQL = `
	SELECT r.name FROM artifact_entitlement ae
	JOIN role r ON r.id = ae.role_id
	WHERE ae.tenant_id = $1 AND ae.artifact_id = $2
	ORDER BY r.name`

// ArtifactRoleGrants reads the grants attached to artifactID without locking
// anything: the read path (a by-id artifact fetch), where the number is advice
// shown to an admin who has not committed to anything yet.
//
// An unknown or cross-tenant artifactID is not an error here, it is zero grants.
// This function is only ever called alongside a real artifact read that has
// already established existence and tenancy (handleGetArtifact's store.GetArtifact,
// handleUpdateArtifact's GetArtifactForUpdate), so re-deriving not-found from a
// grant count would add a second, weaker existence check whose answer is already
// known. A malformed id still maps to ErrNotFound via readGrantNames, because
// the uuid cast is the caller's own contract everywhere else in this package.
func (s *Store) ArtifactRoleGrants(ctx context.Context, tenantID, artifactID string) (ArtifactRoleGrants, error) {
	return s.artifactRoleGrants(ctx, artifactRoleGrantsSQL, tenantID, artifactID)
}

// ArtifactRoleGrantsForUpdate is ArtifactRoleGrants plus `FOR UPDATE OF ae`,
// for the write path: the count that goes into an artifact.update audit record
// must describe what the committing transaction actually did to those grants,
// not a picture taken before it.
//
// Two different races, closed two different ways, and only one of them is this
// clause:
//
//   - A grant ADDED mid-flight is closed by the caller, not here.
//     handleCreateArtifactEntitlement takes ArtifactExistsInTenant's FOR SHARE
//     lock on the artifact row and holds it across its INSERT, so
//     handleUpdateArtifact's GetArtifactForUpdate (FOR UPDATE, same row) cannot
//     be granted until that inserter commits. Once it is granted, this read runs
//     in a fresh READ COMMITTED statement snapshot that already contains the new
//     grant. That is exactly the ordering v1.24.0 established for DeleteRole
//     (see its doc comment): take the conflicting lock FIRST, then read. A row
//     that does not exist yet cannot be locked, so no clause here could have
//     done it.
//
//   - A grant REVOKED mid-flight is what this clause closes.
//     handleDeleteArtifactEntitlement touches no artifact row at all, so the
//     lock above does not stop it. Without FOR UPDATE, a revoke committing
//     between this read and the caller's COMMIT leaves the audit naming a role
//     whose grant no longer exists. With it, an in-flight revoke is waited out
//     (and its row correctly disappears from the result), and a revoke arriving
//     after this read blocks until the caller commits, which makes what is
//     reported true as of that commit.
//
// The lock is `OF ae` deliberately, NOT a bare FOR UPDATE, which would also lock
// the joined role rows. DeleteRole holds a role row FOR UPDATE and then cascades
// into artifact_entitlement; a caller holding artifact_entitlement rows and
// waiting on a role row would close that cycle into a deadlock. The uncovered
// case is therefore a concurrent role DELETION cascading a grant away just after
// this read, and it is left uncovered on purpose: that deletion writes its own
// role.delete audit event naming the artifacts it revoked (admin_roles.go), so
// the operator question this feature exists to answer is still answerable from
// the audit log, by the record of the transaction that actually did it.
func (s *Store) ArtifactRoleGrantsForUpdate(ctx context.Context, tenantID, artifactID string) (ArtifactRoleGrants, error) {
	return s.artifactRoleGrants(ctx, artifactRoleGrantsSQL+"\n\tFOR UPDATE OF ae", tenantID, artifactID)
}

func (s *Store) artifactRoleGrants(ctx context.Context, sql, tenantID, artifactID string) (ArtifactRoleGrants, error) {
	// MaxGrantNames, like DeleteRole's two survivor lists and unlike its
	// virtual-key list: these role names describe role rows this call does
	// not touch, so a capped list is one admin-list call away from being
	// completed rather than a permanent loss.
	names, n, truncated, err := s.readGrantNames(ctx, sql, tenantID, artifactID, MaxGrantNames)
	if err != nil {
		return ArtifactRoleGrants{}, fmt.Errorf("artifact role grants: %w", err)
	}
	return ArtifactRoleGrants{Count: n, RoleNames: names, Truncated: truncated}, nil
}
