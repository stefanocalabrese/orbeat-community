package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Artifact is a catalog entry for a distributable Claude Code artifact
// (a skill or a subagent). Content is the on-disk file body (frontmatter + body).
// MemoryScope applies to subagents only ("" means no native memory).
type Artifact struct {
	ID          string
	TenantID    string
	Type        string // skill | subagent
	Name        string // slug
	Description string
	Content     string
	MemoryScope string // "" | user | project | local
	MemorySeed  string // governed seed memory body ("" = no seed); delivery gated to user/project-scope subagents in the API layers
	Version     string
	Visibility  string // org | role

	// Approval governance (Phase 4). ApprovalState tracks the WORKING copy;
	// the Approved* fields are the frozen snapshot served to clients ("" = NULL,
	// i.e. no approved snapshot exists yet).
	ApprovalState       string // draft | pending | approved | rejected
	ApprovedContent     string
	ApprovedMemorySeed  string
	ApprovedMemoryScope string

	// ApprovedType/Name/Visibility are the distributed IDENTITY, frozen at
	// approval alongside ApprovedContent (migration 00016). Type and Name are
	// the file path on every developer machine and Visibility picks the
	// channel, so an edit to any of them dirties the working copy to draft and
	// reaches developers only once a second admin approves it. All three are
	// NULL exactly when ApprovedContent is, enforced by the
	// artifact_approved_identity_complete CHECK.
	ApprovedType       string
	ApprovedName       string
	ApprovedVisibility string

	SubmittedBy  string
	SubmittedAt  *time.Time
	ApprovedBy   string
	ApprovedAt   *time.Time
	RejectReason string
	ScanFindings json.RawMessage // jsonb array of govern.Finding; store stays govern-free

	// ScanFindingsDigest is a stable digest over ScanFindings, computed and
	// stored at submit (migration 00028). "" means NULL: a draft that has
	// never been submitted, or a row submitted before this column existed.
	// The store does not compute it and does not interpret it; it is govern's
	// digest function (internal/govern), passed in whole by the caller.
	ScanFindingsDigest string

	// FindingsAckDigest/FindingsAckBy/FindingsAckAt are the AUTHOR's
	// acknowledgment of a specific findings digest: the digest they read, who
	// they are, and when. All three are "" / nil together (NULL together in
	// the schema, enforced by artifact_findings_ack_complete) exactly when no
	// acknowledgment has been recorded, which is the state every existing row
	// reads as after the additive migration and the state a fresh draft is
	// always created in.
	//
	// FindingsAckDigest is bound to the digest it was read against rather
	// than being a boolean, because the scanner is nondeterministic and a
	// withdraw-then-resubmit re-scans: a plain "acknowledged" flag would
	// silently survive that re-scan and end up describing findings the
	// author never saw. FindingsAckDigest != ScanFindingsDigest is a valid
	// state, not a violation, and comparing the two is a caller's job, not
	// this struct's.
	//
	// It is no longer an ORDINARY one, though, and the difference matters to
	// anyone writing a test: since 2026-08-28 SetArtifactSubmitted clears all
	// three columns, so a re-scan can no longer LEAVE a stale acknowledgment
	// behind, and the mismatch is reachable only by calling
	// SetArtifactFindingsAcknowledged with a digest of the caller's choosing,
	// which is what this package's own tests do. Callers must still compare;
	// they simply cannot expect the handlers to produce a mismatch for them.
	FindingsAckDigest string
	FindingsAckBy     string
	FindingsAckAt     *time.Time

	// RowVersion is the optimistic-concurrency token. Incremented by the
	// artifact_bump_row_version trigger (migration 00013) on EVERY update, so no
	// statement can change the row without invalidating an outstanding client's
	// precondition.
	RowVersion int64

	// HasApproved reports whether a live approved snapshot exists. It is selected
	// as its own column (approved_content IS NOT NULL) rather than derived from
	// ApprovedContent != "", because the slim list projection omits
	// approved_content — deriving it there would report every listed artifact as
	// unapproved, silently blanking the portal's "Live" badge.
	//
	// Populated by scanArtifact only. The distribution projection
	// (scanDistArtifact, used by ListActiveOrgArtifacts and the role-entitled
	// dist query) does not select has_approved and leaves this field zero —
	// every row it returns has a live snapshot by construction (that's the
	// query's own WHERE clause), so no dist consumer reads this field.
	HasApproved bool

	// Revision is MAX(artifact_revision.revision_num) for this artifact: the
	// ordinal, server-assigned version identity of the approved snapshot the
	// distribution queries are handing out. It is NOT artifact.version, which
	// is admin-authored free text no distribution query has ever read.
	//
	// Populated by scanDistArtifact ONLY, the same way HasApproved is
	// populated by scanArtifact only. artifactCols does not project it, so a
	// consumer reading Revision off a GetArtifact or a ListArtifactsPage
	// result silently gets 0. Zero also means "no revision row exists", which
	// is unreachable through any API path (00007's Up grandfathers every
	// approved artifact as revision 1, SetArtifactApproved appends one per
	// approval, and insertRevision's prune keeps the newest for any keep >= 1)
	// but is reachable by direct SQL. insertRevision numbers from 1, so 0 is
	// never a real revision and the two cases are safe to conflate.
	Revision int

	// MinRevisionNum is the admin's minimum-revision FLOOR for this artifact
	// (migration 00018): the oldest revision a developer machine is allowed to
	// keep serving, overriding any pin she set locally. 0 means NO FLOOR,
	// unambiguous because insertRevision numbers from 1.
	//
	// It is a policy an admin WRITES, and it is not OldestRevision below. The
	// two names are one word apart and mean opposite things: this is what an
	// admin decided, OldestRevision is what the prune happened to leave. A
	// floor is free to point below every surviving revision; the clamp
	// resolves against the window that exists.
	//
	// Populated by BOTH scans. artifactCols and artifactSlimCols project it
	// (an int, not a payload the slim projection exists to drop) and
	// distArtifactCols projects it for the clamp, so unlike every other field
	// in this block there is no read path that silently yields a zero for it.
	MinRevisionNum int

	// OldestRevision is MIN(artifact_revision.revision_num) for this artifact:
	// the oldest revision that still EXISTS. It is 1 until
	// ORBEAT_ARTIFACT_REVISION_KEEP prunes the chain and rises from there, so
	// treating it as a constant 1 is wrong on exactly the installs that prune.
	// With Revision it is the window a client-side pin is clamped into:
	// OldestRevision is the floor of what can be served, Revision the ceiling.
	//
	// Populated by scanDistArtifact ONLY, the same way Revision is, so a
	// consumer reading it off a GetArtifact or a ListArtifactsPage result
	// silently gets 0. Zero also means "no revision row exists", which is the
	// same unreachable-through-any-API-path case Revision documents above and
	// is safe to conflate for the same reason.
	OldestRevision int

	// TargetTags is the set of PROJECT TAGS a rule applies to, and nil means
	// every registered project, which is what every rule shipped before
	// migration 00024 does. Rule-only, enforced by that migration's CHECK.
	//
	// The tags are declared by the developer on their own machine
	// (`orbeat-sync project add ~/work/api --tag go`) and orbeat never learns
	// them: the admin says what KIND of project a rule is for, the developer
	// says what kind theirs are. An empty non-nil slice is normalised to NULL
	// on write (NULLIF against '{}'), because "targets nothing" and "targets
	// everything" must not both be expressible.
	TargetTags []string
	// ApprovedTargetTags is the snapshotted targeting that is actually being
	// distributed, the same way ApprovedVisibility is. Re-targeting an approved
	// rule changes WHO RECEIVES IT, so it waits for an approval.
	ApprovedTargetTags []string

	// RuleScope is "project" (or empty, which means the same) or "global".
	// A project rule lands in each registered project's AGENTS.md; a global one
	// lands in the user-level instruction files every project inherits, which
	// is where an instruction about the DEVELOPER belongs rather than one about
	// a repository. Rule-only, enforced by migration 00025's CHECK.
	RuleScope string
	// ApprovedRuleScope is the snapshotted scope actually in effect, for the
	// same reason ApprovedVisibility and ApprovedTargetTags exist: flipping a
	// rule to global changes it from reaching whoever registered a matching
	// project to reaching everyone who syncs.
	ApprovedRuleScope string
}

const artifactCols = `id::text, tenant_id::text, type, name, description, content,
	memory_scope, memory_seed, version, visibility,
	approval_state, approved_content, approved_memory_seed, approved_memory_scope,
	approved_type, approved_name, approved_visibility,
	submitted_by, submitted_at, approved_by, approved_at, reject_reason, scan_findings,
	scan_findings_digest, findings_ack_digest, findings_ack_by, findings_ack_at,
	approved_content IS NOT NULL AS has_approved, row_version, min_revision_num,
	target_tags, approved_target_tags, rule_scope, approved_rule_scope`

// artifactSlimCols is artifactCols with the four heavy payload columns replaced
// by typed NULL / empty-string placeholders: content (<=64 KiB), memory_seed
// (<=16 KiB), approved_content (<=64 KiB) and approved_memory_seed (<=16 KiB;
// caps per govern.MaxContentBytes / govern.MaxSeedBytes) — 64+16+64+16 =
// ~160 KiB per row, which a 100-row page would turn into ~16 MB.
//
// The column ORDER and COUNT must stay identical to artifactCols: both are
// scanned by scanArtifact. has_approved is computed from approved_content even
// though the column itself is not selected, so the flag survives the slimming.
//
// approved_type/approved_name/approved_visibility carry REAL values here, and
// so does min_revision_num, and that asymmetry with approved_content is
// deliberate, not an oversight: the three identity columns are slug-sized and
// the floor is an int, none of them the 64 KiB payload this projection exists
// to drop, and the list row needs them without ?include=content, for the
// pending-identity badge (the answer to "why is the file on my machine still
// called foo") and for the artifact's minimum-revision floor respectively.
// scan_findings_digest/findings_ack_digest/findings_ack_by/findings_ack_at
// (migration 00028) carry real values for the same reason: a digest and an
// acknowledgment are slug/timestamp-sized, not payload, and a list row needs
// them to show whether a pending artifact still needs its author's
// acknowledgment without a second ?include=content round trip.
const artifactSlimCols = `id::text, tenant_id::text, type, name, description, '' AS content,
	memory_scope, NULL::text AS memory_seed, version, visibility,
	approval_state, NULL::text AS approved_content, NULL::text AS approved_memory_seed, approved_memory_scope,
	approved_type, approved_name, approved_visibility,
	submitted_by, submitted_at, approved_by, approved_at, reject_reason, scan_findings,
	scan_findings_digest, findings_ack_digest, findings_ack_by, findings_ack_at,
	approved_content IS NOT NULL AS has_approved, row_version, min_revision_num,
	target_tags, approved_target_tags, rule_scope, approved_rule_scope`

func scanArtifact(row interface{ Scan(...any) error }) (Artifact, error) {
	var a Artifact
	var memScope, memSeed, appContent, appSeed, appScope, subBy, appBy, rejReason *string
	var appType, appName, appVis *string
	var ruleScope, appRuleScope *string
	var scanDigest, ackDigest, ackBy *string
	err := row.Scan(&a.ID, &a.TenantID, &a.Type, &a.Name, &a.Description,
		&a.Content, &memScope, &memSeed, &a.Version, &a.Visibility,
		&a.ApprovalState, &appContent, &appSeed, &appScope,
		&appType, &appName, &appVis,
		&subBy, &a.SubmittedAt, &appBy, &a.ApprovedAt, &rejReason, &a.ScanFindings,
		&scanDigest, &ackDigest, &ackBy, &a.FindingsAckAt,
		&a.HasApproved, &a.RowVersion, &a.MinRevisionNum, &a.TargetTags, &a.ApprovedTargetTags,
		&ruleScope, &appRuleScope)
	if err != nil {
		return Artifact{}, err
	}
	for dst, src := range map[*string]*string{
		&a.MemoryScope: memScope, &a.MemorySeed: memSeed,
		&a.ApprovedContent: appContent, &a.ApprovedMemorySeed: appSeed,
		&a.ApprovedMemoryScope: appScope, &a.SubmittedBy: subBy,
		&a.ApprovedType: appType, &a.ApprovedName: appName,
		&a.ApprovedVisibility: appVis,
		&a.ApprovedBy:         appBy, &a.RejectReason: rejReason,
		&a.ScanFindingsDigest: scanDigest, &a.FindingsAckDigest: ackDigest,
		&a.FindingsAckBy: ackBy,
	} {
		if src != nil {
			*dst = *src
		}
	}
	// Postgres' jsonb text output inserts whitespace after ':' and ',' (its
	// canonical pretty form), which is insignificant per the JSON grammar but
	// would otherwise make ScanFindings byte-compare unstable for callers.
	// Canonicalize to the minimal form so it round-trips deterministically.
	if len(a.ScanFindings) > 0 {
		var buf bytes.Buffer
		if err := json.Compact(&buf, a.ScanFindings); err == nil {
			a.ScanFindings = buf.Bytes()
		}
	}
	if ruleScope != nil {
		a.RuleScope = *ruleScope
	}
	if appRuleScope != nil {
		a.ApprovedRuleScope = *appRuleScope
	}
	return a, nil
}

func (s *Store) CreateArtifact(ctx context.Context, a Artifact) (Artifact, error) {
	row := s.db.QueryRow(ctx, `
		INSERT INTO artifact (tenant_id, type, name, description, content, memory_scope, memory_seed, version, visibility, target_tags, rule_scope)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,COALESCE(NULLIF($9,''),'org'),NULLIF($10,'{}'::text[]),NULLIF($11,''))
		RETURNING `+artifactCols,
		a.TenantID, a.Type, a.Name, a.Description, a.Content, a.MemoryScope, a.MemorySeed, a.Version, a.Visibility, a.TargetTags, a.RuleScope)
	created, err := scanArtifact(row)
	if err != nil {
		return Artifact{}, fmt.Errorf("create artifact: %w", err)
	}
	return created, nil
}

// GetArtifact fetches an artifact scoped to tenantID; a cross-tenant or
// unknown id returns ErrNotFound (the SQL filter makes this the only
// possible outcome, so a caller cannot forget the tenant check).
func (s *Store) GetArtifact(ctx context.Context, tenantID, id string) (Artifact, error) {
	a, err := scanArtifact(s.db.QueryRow(ctx, `SELECT `+artifactCols+` FROM artifact WHERE tenant_id=$1 AND id=$2`, tenantID, id))
	if err != nil {
		if idCastNotFound(err) {
			return Artifact{}, ErrNotFound
		}
		return Artifact{}, fmt.Errorf("get artifact: %w", err)
	}
	return a, nil
}

// ApprovedIdentityUniqueIndex is the partial unique index migration 00016 put
// on (tenant_id, approved_type, approved_name) WHERE approved_content IS NOT
// NULL: the namespace that actually reaches a developer's disk.
//
// Exported because a 23505 against THIS index means something the API layer
// has to say differently from every other duplicate, and the constraint name
// is the only thing that tells the two apart: the live
// UNIQUE (tenant_id, type, name) from 00003 fires on a name the admin can see
// in their own list, this one fires on a name that is free in that list and
// taken in the one that ships.
const ApprovedIdentityUniqueIndex = "artifact_tenant_approved_identity_uniq"

// ApprovedIdentityConflict reports a unique violation against
// ApprovedIdentityUniqueIndex and carries the (Type, Name) pair the refused
// write was trying to distribute under.
//
// The pair travels WITH the error because the writer is the last layer that
// can name it. Postgres aborts the whole transaction on a 23505, so no
// statement after it can look anything up, and RollbackArtifact's caller
// cannot recompute the pair either: it is the target revision's identity when
// that revision recorded one and the artifact's current approved (or, on a
// withdrawn row, live) identity when it did not. A second copy of that
// fallback in the handler is a copy that drifts from the one that decides.
type ApprovedIdentityConflict struct {
	Type string
	Name string
	Err  error
}

func (e *ApprovedIdentityConflict) Error() string {
	return fmt.Sprintf("approved identity %s/%s is already distributed by another artifact: %v", e.Type, e.Name, e.Err)
}

// Unwrap keeps the underlying *pgconn.PgError reachable, so errors.As still
// finds the 23505 through this wrapper and the API's generic constraint arm
// stays a floor UNDER this one rather than being bypassed by it.
func (e *ApprovedIdentityConflict) Unwrap() error { return e.Err }

// asApprovedIdentityConflict wraps err with the identity the statement was
// writing when err is a unique violation against ApprovedIdentityUniqueIndex.
// Everything else, a 23505 against a DIFFERENT constraint included, passes
// through untouched.
func asApprovedIdentityConflict(err error, artType, name string) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != ApprovedIdentityUniqueIndex {
		return err
	}
	return &ApprovedIdentityConflict{Type: artType, Name: name, Err: err}
}

// ArtifactDistributingAs returns the id and the LIVE name of the artifact that
// currently distributes under (artType, name) in the tenant, or ErrNotFound
// when none does.
//
// It exists for one sentence in one error body, and that sentence is the whole
// point of the 409 it feeds: the admin is looking at a list where no row is
// called foo, because the row that holds foo in the DISTRIBUTED namespace is
// called bar in the only namespace the list shows them. Naming it is the
// difference between an error they can act on and one they cannot.
//
// At most one row can match: ApprovedIdentityUniqueIndex is exactly this
// predicate.
func (s *Store) ArtifactDistributingAs(ctx context.Context, tenantID, artType, name string) (id, liveName string, err error) {
	err = s.db.QueryRow(ctx, `SELECT id::text, name FROM artifact
		WHERE tenant_id=$1 AND approved_type=$2 AND approved_name=$3 AND approved_content IS NOT NULL`,
		tenantID, artType, name).Scan(&id, &liveName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("artifact distributing as %s/%s: %w", artType, name, err)
	}
	return id, liveName, nil
}

// artifactKeys is artifact's sort order (id appended by keysetTail).
var artifactKeys = []sortKey{{Col: "type", Cast: "text"}, {Col: "name", Cast: "text"}}

// ArtifactCursor is the keyset position just after a, walked in direction
// desc (?order), see RoleCursor's doc comment (rbac.go) for why desc must
// match the direction that produced a.
func ArtifactCursor(a Artifact, desc bool) ListCursor {
	return ListCursor{Keys: []string{a.Type, a.Name}, ID: a.ID, Sort: sortIdentity("artifact", artifactKeys, desc)}
}

// ArtifactPageOpts narrows a paginated artifact list.
type ArtifactPageOpts struct {
	State          string      // "" = no approval-state filter
	Cursor         *ListCursor // nil = first page
	Limit          int         // <= 0 = no limit
	IncludeContent bool        // false = the heavy payload columns are omitted
	Desc           bool        // true = ?order=desc: reverse (type, name, id) uniformly
	// Search is an optional ?q= substring match against name (not type: the
	// allowlisted ?sort column here, artifactSortName, is deliberately not
	// the same column search targets, see artifactPageSQL's own comment).
	// "" = no filter. Case-insensitive and unindexed by design, see
	// likeSearchArg's doc comment (paging.go).
	Search string
}

// artifactPageSQL builds the tenant-scoped keyset page query and returns the
// COMPLETE bind-ordered arg list. Split out from ListArtifactsPage so the
// index-usage test can EXPLAIN the exact SQL that runs in production.
func artifactPageSQL(tenantID string, o ArtifactPageOpts) (string, []any, error) {
	cols := artifactSlimCols
	if o.IncludeContent {
		cols = artifactCols
	}
	// $2 is the state filter: NULL (no filter) or the state to match. The OR
	// guard keeps artifact_tenant_state_type_name_id_idx available to the
	// planner under the CUSTOM plan pgx actually runs (QueryExecModeCacheStatement
	// re-plans per exec until Postgres considers a generic plan around the 6th
	// execution of a prepared statement) — verified cost-for-cost equal to a
	// plain-equality control. A FORCED generic plan does lose the index (a
	// literal $2 defeats the OR's constant-folding), but plan_cache_mode stays
	// at its default `auto`, which only switches to generic when it is not more
	// expensive than custom — it is here, so it never does (8 consecutive
	// EXPLAIN EXECUTEs all stayed custom). The keyset ORDER BY is index-driven
	// under both plan kinds either way.
	//
	// $3 is the ?q= substring search filter (docs/plans/orbeat-admin-search-
	// sort-2026-08-27.md Task 4), the same NULL-means-no-filter shape as $2
	// above, see likeSearchArg's doc comment (paging.go). It matches name,
	// deliberately NOT type: type is artifactSortName, the one ?sort value
	// this list allowlists, but its only possible values are "skill",
	// "subagent" and "rule", a three-way enum a substring search over is not
	// a useful search surface, where name is the free-text identifier an
	// admin actually recognizes an artifact by. Unlike $2, there is no OR-guard
	// index concern to preserve here: ILIKE with a leading wildcard cannot use
	// an index regardless of how the predicate is shaped (likeSearchArg's own
	// comment), so this is a plain, unconditionally-scanned OR, nothing to
	// keep a plan choice stable for.
	base := `SELECT ` + cols + ` FROM artifact
		WHERE tenant_id = $1 AND ($2::text IS NULL OR approval_state = $2)
		  AND ($3::text IS NULL OR name ILIKE $3)`
	tail, tailArgs, err := keysetTail("artifact", artifactKeys, o.Desc, o.Cursor, o.Limit, 3)
	if err != nil {
		return "", nil, err
	}
	var stateArg any
	if o.State != "" {
		stateArg = o.State
	}
	return base + tail, append([]any{tenantID, stateArg, likeSearchArg(o.Search)}, tailArgs...), nil
}

// ListArtifactsPage returns up to o.Limit artifacts for a tenant ordered
// (type, name, id), or (type DESC, name DESC, id DESC) when o.Desc is true
// (?order=desc), starting strictly after o.Cursor.
//
// The approval-state filter AND o.Search both run in SQL, not in a Go loop
// over the result: with either filter applied after LIMIT, a page would come
// back short, or empty, while more matches existed (spec §4.1 for state;
// docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 4 for the same
// reasoning applied to search). That is a correctness bug, not a performance
// one.
func (s *Store) ListArtifactsPage(ctx context.Context, tenantID string, o ArtifactPageOpts) ([]Artifact, error) {
	sql, args, err := artifactPageSQL(tenantID, o)
	if err != nil {
		return nil, fmt.Errorf("artifact page cursor: %w", err)
	}
	return s.queryArtifacts(ctx, sql, args...)
}

// distArtifactCols is the minimal projection distribution consumers need
// (marketplace renderer + sync DTO): the APPROVED snapshot, scanned into the
// working field positions so downstream code is unchanged.
//
// Every column is an approved_* one, identity included. Migration 00016 gave
// name, type and visibility the second copy content has had since 00006, so a
// rename reaches developer machines when a second admin approves it rather
// than the instant it is saved. That is what makes the artifact's file path
// (skills/<name>/SKILL.md, agents/<name>.md) a reviewed decision.
//
// id and the revision aggregate are the two columns that are NOT part of the
// approved snapshot, and they are here because a developer machine cannot
// report a version it was never told. id is the stable registry key a rename
// cannot break; the aggregate is the ordinal version identity (see
// Artifact.Revision). The aggregate is a correlated subquery rather than a
// LEFT JOIN because it has to live INSIDE this const: a join would have to be
// hand-added to two separate FROM clauses that the const cannot reach, which
// is the hand-copied-projection defect described below, reintroduced one level
// up. It costs one index lookup per row, served by 00007's
// UNIQUE (artifact_id, revision_num).
//
// min_revision_num and the MIN aggregate are the clamp's other two inputs
// (docs/specs/2026-08-22-orbeat-artifact-version-pinning-design.md §4.2): the
// floor an admin wrote, and the oldest revision the prune has left alive.
// With the MAX above they are the whole window a client-side pin is resolved
// into, and all three have to arrive on the same row or the clamp has nothing
// to clamp against. The MIN is a correlated subquery for the same reason the
// MAX is and reads the same index.
//
// THE INNER PREDICATE QUALIFIES BOTH SIDES, and for these two subqueries that
// is not the 42702 rule below. artifact_revision has an id column of its own,
// so a predicate written `WHERE artifact_id = id` resolves the bare id to the
// INNER table rather than to the outer artifact, which decorrelates the
// subquery entirely: it compares each revision row's artifact_id to that same
// row's own id, matches nothing, and both aggregates come back NULL for every
// artifact, folded to 0 below. Measured, not reasoned: the mutant raises NO
// error and every row reports an empty revision window. 42702 fails loudly;
// this one is silent, and 0 is exactly the value scanDistArtifact already
// treats as "no revision row exists".
//
// BOTH distribution queries SELECT this one const: ListActiveOrgArtifacts
// below (Channel 1) and ListEntitledArtifacts (artifact_entitlement.go,
// Channel 2), which used to hand-copy the same five columns with an `a.`
// prefix. Sharing it is load-bearing rather than tidy, because what those two
// queries also share is a POSITIONAL scan (scanDistArtifact): a column added,
// dropped or reordered in one copy and not the other is a runtime pgx scan
// error on a live sync, never a compile error.
//
// That sharing is why BOTH queries pay for min_revision_num and the MIN
// aggregate while Channel 1 reads neither (nor Revision: only the sync DTO in
// internal/api/sync.go does). It is the right price. The identity slice
// deleted ListEntitledArtifacts' hand-copied projection precisely because a
// divergent copy is a runtime scan error on a live sync rather than a compile
// error, so handing the entitled query its own projection back to save two
// ignored ints would rebuild the defect that slice removed.
//
// EVERY COLUMN IS TABLE-QUALIFIED, and that is a correctness requirement, not
// house style. artifact_entitlement's columns are id, tenant_id, role_id,
// artifact_id and created_at (migration 00004), so a bare `id` here is an
// AMBIGUOUS COLUMN REFERENCE (SQLSTATE 42702) in the entitled query, which
// joins those two tables, and legal in the org query, which does not. The
// positional scan the two queries share means that asymmetry surfaces as a
// failed query on a live sync and never at build time. ListEntitledArtifacts
// therefore names the table rather than aliasing it to `a`, so this const's
// `artifact.` prefix resolves in both.
//
// No aliases on the output side either. Nothing selects these by label, and
// `approved_name AS name` would hand every ORDER BY in these queries a bare
// `name` output label to bind to, which is exactly the hazard
// ListActiveOrgArtifacts' comment below describes. Qualifying every key on the
// input side kills that hazard structurally rather than by convention.
//
// One honest limit on the table-qualification rule two paragraphs up, stated
// rather than papered over with a test: it is NOT FALSIFIABLE for
// min_revision_num. That column exists on artifact
// and on no other table either query touches, so dropping its `artifact.`
// prefix compiles and runs correctly in both. What gates the rule is the
// bare-id proof the parity test in artifact_entitlement_test.go already
// carries, and this column rides on it; do not add a test that pretends to
// prove the prefix for a name that cannot break without it.
const distArtifactCols = `artifact.id::text,
	artifact.approved_type, artifact.approved_name, artifact.approved_content,
	artifact.approved_memory_scope, artifact.approved_memory_seed,
	(SELECT MAX(revision_num) FROM artifact_revision WHERE artifact_revision.artifact_id = artifact.id),
	artifact.min_revision_num,
	(SELECT MIN(revision_num) FROM artifact_revision WHERE artifact_revision.artifact_id = artifact.id),
	artifact.approved_target_tags, artifact.approved_rule_scope`

func scanDistArtifact(row interface{ Scan(...any) error }) (Artifact, error) {
	var a Artifact
	var scope, seed *string
	// revision and oldest are nullable: MAX and MIN over zero rows are both
	// NULL, unreachable through any API path but reachable by direct SQL.
	// Folded to 0, which the sync DTO's omitempty then drops (see
	// Artifact.Revision and Artifact.OldestRevision). min_revision_num is NOT
	// NULL in the schema and needs no fold.
	var revision, oldest *int
	var distScope *string
	if err := row.Scan(&a.ID, &a.Type, &a.Name, &a.Content, &scope, &seed,
		&revision, &a.MinRevisionNum, &oldest, &a.ApprovedTargetTags, &distScope); err != nil {
		return Artifact{}, err
	}
	if scope != nil {
		a.MemoryScope = *scope
	}
	if seed != nil {
		a.MemorySeed = *seed
	}
	if revision != nil {
		a.Revision = *revision
	}
	if oldest != nil {
		a.OldestRevision = *oldest
	}
	if distScope != nil {
		a.ApprovedRuleScope = *distScope
	}
	return a, nil
}

func (s *Store) queryDistArtifacts(ctx context.Context, sql string, args ...any) ([]Artifact, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list artifact: %w", err)
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		a, err := scanDistArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListActiveOrgArtifacts returns the artifacts whose APPROVED visibility is
// 'org' and which carry an approved snapshot (the Channel-1 plugin input).
// Every returned field is that snapshot: Type and Name as well as Content,
// MemoryScope and MemorySeed. So a rename, a type change or a flip to role
// visibility changes what this returns only once it has been approved, and the
// published plugin tree never moves a file nobody signed off on.
//
// The WHERE clause reads approved_visibility, and migration 00016 rebuilt
// artifact_tenant_distributable_idx on (tenant_id, approved_visibility) WHERE
// approved_content IS NOT NULL to match. 00010 had keyed it on the live
// column; leaving it there would have kept it maintained on every write and
// serving nothing.
//
// ORDER BY is table-qualified even though it does not have to be today.
// distArtifactCols projects approved_type/approved_name uncast and unaliased,
// so a bare ORDER BY would bind to output labels that happen to name the same
// columns. That coincidence is the whole v1.22.0 defect: PostgreSQL resolves a
// bare ORDER BY key against OUTPUT LABELS before table columns, so `SELECT
// id::text ... ORDER BY id` sorted on the text cast and no uuid index could
// serve it, results were never wrong, and it shipped inert in audit.go from
// Phase 1. Qualifying costs nothing and stops the correctness of this sort
// depending on the projection never gaining a cast or an alias.
func (s *Store) ListActiveOrgArtifacts(ctx context.Context, tenantID string) ([]Artifact, error) {
	return s.queryDistArtifacts(ctx, `SELECT `+distArtifactCols+` FROM artifact
		WHERE tenant_id=$1 AND approved_visibility='org' AND approved_content IS NOT NULL
		ORDER BY artifact.approved_type, artifact.approved_name`, tenantID)
}

// ListActiveOrgRules returns the tenant's approved org-visibility RULES, the
// one artifact type with no Channel 1 to ship through.
//
// Every other type reaches an org audience through the marketplace plugin, and
// ListActiveOrgArtifacts above is that query. A rule is Channel-2 only
// (v1.14.0): marketplace.RenderArtifactsPlugin's type switch has no `rule` case
// and drops it in a default whose comment claims the type is constrained
// upstream, which stopped being true when `rule` became a valid type. So an org
// rule was returned by the Channel-1 query, dropped before it reached a file,
// and excluded from Channel 2 for being org visibility: it reached nobody, from
// the DEFAULT value of the column.
//
// The type filter is in SQL rather than a Go loop over ListActiveOrgArtifacts
// for the reason v1.22.0 established: a filter applied after the query is a
// filter the planner cannot use and, wherever a LIMIT exists, is applied to the
// wrong set. There is no LIMIT here today, which is exactly when the habit is
// cheap to keep.
//
// `approved_content IS NOT NULL` is PROVABLY REDUNDANT here and kept anyway.
// Migration 00016 carries `CHECK ((approved_content IS NULL) = (approved_visibility
// IS NULL))`, so `approved_visibility='org'` already excludes every unapproved
// row; a mutant that deletes this clause changes no result, and no test can
// fail for it. It stays because the sibling query above shares a POSITIONAL
// scan with this one and differing WHERE clauses between the two are how that
// pair drifts. Do not read it as the guard that keeps drafts out of
// distribution: the CHECK is.
func (s *Store) ListActiveOrgRules(ctx context.Context, tenantID string) ([]Artifact, error) {
	return s.queryDistArtifacts(ctx, `SELECT `+distArtifactCols+` FROM artifact
		WHERE tenant_id=$1 AND approved_visibility='org' AND approved_type='rule'
		  AND approved_content IS NOT NULL
		ORDER BY artifact.approved_type, artifact.approved_name`, tenantID)
}

func (s *Store) queryArtifacts(ctx context.Context, sql string, args ...any) ([]Artifact, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list artifact: %w", err)
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateArtifact replaces an artifact's editable fields. expected is the
// row_version the caller last read; the UPDATE only applies if it still
// matches. A single statement must distinguish "doesn't exist" (ErrNotFound)
// from "exists but the version is stale" (ErrVersionMismatch): a plain
// UPDATE...RETURNING cannot tell those apart, since both return zero rows —
// and conflating them matters once the API layer maps ErrVersionMismatch to
// HTTP 412, because a nonexistent or cross-tenant id must stay 404 (spec
// §5.2), not 412. Mirrors UpdateMCPServer's CTE shape for exactly that
// reason.
//
// The row_version predicate is a BACKSTOP, not the decisive check for
// handleUpdateArtifact: that handler already holds a FOR UPDATE lock via
// GetArtifactForUpdate before ever reaching this call, so the row cannot
// change between that read and this UPDATE within the same transaction —
// there is no race window for that caller regardless of what it does with
// the version it read. (Task 7 adds the actual rejection of a stale
// CLIENT-supplied version, compared in Go before this call runs; today the
// handler passes the just-read current.RowVersion straight through, which
// trivially always matches — see admin_artifacts.go.) This predicate exists
// for any caller — present or future — that does not hold the lock, and this
// function must defend against a malformed id itself: it cannot assume a
// caller has already validated it.
func (s *Store) UpdateArtifact(ctx context.Context, a Artifact, expected int64) (Artifact, error) {
	const q = `
		WITH cur AS (SELECT 1 FROM artifact WHERE tenant_id=$1 AND id=$2),
		     upd AS (
		       UPDATE artifact SET type=$3, name=$4, description=$5, content=$6,
		         memory_scope=NULLIF($7,''), memory_seed=NULLIF($8,''), version=$9,
		         visibility=COALESCE(NULLIF($10,''),'org'),
		         target_tags=NULLIF($12,'{}'::text[]), rule_scope=NULLIF($13,''),
		         approval_state='draft', updated_at=now()
		       WHERE tenant_id=$1 AND id=$2 AND row_version=$11
		       RETURNING 1
		     )
		SELECT (SELECT count(*) FROM cur), (SELECT count(*) FROM upd)`
	var existsCnt, updCnt int
	err := s.db.QueryRow(ctx, q,
		a.TenantID, a.ID, a.Type, a.Name, a.Description, a.Content, a.MemoryScope, a.MemorySeed, a.Version, a.Visibility, expected, a.TargetTags, a.RuleScope,
	).Scan(&existsCnt, &updCnt)
	if err != nil {
		// Same reasoning as UpdateMCPServer: the SELECT above is two scalar
		// subqueries with no FROM clause, so it always returns exactly one row
		// (idCastNotFound's pgx.ErrNoRows arm never fires here); its 22P02 arm
		// is what matters, since id=$2 still undergoes the uuid cast in both
		// CTEs' WHERE clauses.
		if idCastNotFound(err) {
			return Artifact{}, ErrNotFound
		}
		return Artifact{}, fmt.Errorf("update artifact: %w", err)
	}
	if existsCnt == 0 {
		return Artifact{}, ErrNotFound
	}
	if updCnt == 0 {
		return Artifact{}, ErrVersionMismatch
	}
	updated, err := s.GetArtifact(ctx, a.TenantID, a.ID)
	if err != nil {
		return Artifact{}, fmt.Errorf("update artifact: reread: %w", err)
	}
	return updated, nil
}

// SetArtifactMinRevision writes the admin's minimum-revision floor
// (migration 00018) and NOTHING ELSE. expected is the row_version the caller
// last read, with the same three outcomes UpdateArtifact has: ErrNotFound when
// no such row exists for this tenant, ErrVersionMismatch when it exists and
// the version is stale, the re-read row otherwise.
//
// IT MUST NOT TOUCH approval_state, AND THAT IS THE WHOLE REASON THIS EXISTS
// RATHER THAN A FIELD ON UpdateArtifact. UpdateArtifact, the function directly
// above, sets approval_state='draft' unconditionally in its UPDATE, so
// folding the floor into it would knock an artifact back to draft the moment
// an admin raised the floor right after approving a security fix. The approved
// snapshot survives that, so distribution would look correct and the only
// symptom would be a Review queue that had quietly grown a row nobody
// submitted. TestSettingTheFloorDoesNotUnapprove is the gate.
//
// The CTE shape is UpdateArtifact's, character for character in structure and
// for its reason: a plain UPDATE ... RETURNING returns zero rows for BOTH
// "does not exist" and "exists but stale", and the API layer has to answer 404
// for the first and 412 for the second. idCastNotFound still maps a malformed
// id (22P02, raised by the uuid cast in both CTEs' WHERE clauses) to
// ErrNotFound rather than letting it become a 500.
//
// The CHECK (min_revision_num >= 0) from 00018 is the backstop under the API's
// own validation, not a substitute for it: a negative floor arriving here is a
// 500, which is the correct answer to a caller that skipped validation.
// Nothing bounds the floor from ABOVE in the schema, because MAX(revision_num)
// is not a column; that bound belongs to the caller, which reads it inside the
// same locked transaction (see handleSetArtifactMinRevision).
//
// SHARED, not artifact.go's .ee sibling, because internal/store splits by
// TABLE and by feature rather than by the edition of a caller:
// min_revision_num is an artifact column that BOTH editions' artifactCols and
// artifactSlimCols project and that both distribution queries read through
// distArtifactCols. Only the ROUTE that writes it is Enterprise-only
// (admin_artifact_min_revision.ee.go), and a generated Community tree compiles
// this method without ever calling it, exactly as it does insertRevision's.
func (s *Store) SetArtifactMinRevision(ctx context.Context, tenantID, id string, minRevision int, expected int64) (Artifact, error) {
	const q = `
		WITH cur AS (SELECT 1 FROM artifact WHERE tenant_id=$1 AND id=$2),
		     upd AS (
		       UPDATE artifact SET min_revision_num=$3, updated_at=now()
		       WHERE tenant_id=$1 AND id=$2 AND row_version=$4
		       RETURNING 1
		     )
		SELECT (SELECT count(*) FROM cur), (SELECT count(*) FROM upd)`
	var existsCnt, updCnt int
	if err := s.db.QueryRow(ctx, q, tenantID, id, minRevision, expected).Scan(&existsCnt, &updCnt); err != nil {
		if idCastNotFound(err) {
			return Artifact{}, ErrNotFound
		}
		return Artifact{}, fmt.Errorf("set artifact min revision: %w", err)
	}
	if existsCnt == 0 {
		return Artifact{}, ErrNotFound
	}
	if updCnt == 0 {
		return Artifact{}, ErrVersionMismatch
	}
	updated, err := s.GetArtifact(ctx, tenantID, id)
	if err != nil {
		return Artifact{}, fmt.Errorf("set artifact min revision: reread: %w", err)
	}
	return updated, nil
}

// GetArtifactForUpdate reads a tenant-scoped artifact with a row lock (FOR
// UPDATE); callers use it inside a tx to make a race-free precondition check
// before a transition.
func (s *Store) GetArtifactForUpdate(ctx context.Context, tenantID, id string) (Artifact, error) {
	a, err := scanArtifact(s.db.QueryRow(ctx,
		`SELECT `+artifactCols+` FROM artifact WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenantID, id))
	if err != nil {
		if idCastNotFound(err) {
			return Artifact{}, ErrNotFound
		}
		return Artifact{}, fmt.Errorf("get artifact for update: %w", err)
	}
	return a, nil
}

// transition runs an UPDATE ... RETURNING that all six of its callers build
// with `WHERE tenant_id=$1 AND id=$2` (id the only user-supplied uuid cast —
// idCastNotFound's own precondition, verified against every call site: the
// remaining bind params are text/jsonb, never a second uuid cast). It maps
// BOTH "no row matched" (pgx.ErrNoRows) and "the id was never a real uuid"
// (Postgres 22P02) to ErrNotFound, via idCastNotFound rather than a bare
// errors.Is(err, pgx.ErrNoRows) check — audit B37: the bare check left a
// malformed id falling through to the generic %w wrap below, a 500 instead
// of a 404. Unreachable through any handler today (every caller runs
// GetArtifactForUpdate first, which already 404s a malformed id before
// transition is ever called — see TestArtifactTransitionMalformedIDIsNotFound
// for the direct, caller-independent proof), but this function cannot rely
// on that: it is called directly by tests and, like every store function in
// this package, must defend against a malformed id itself.
func (s *Store) transition(ctx context.Context, sql string, args ...any) (Artifact, error) {
	a, err := scanArtifact(s.db.QueryRow(ctx, sql, args...))
	if err != nil {
		if idCastNotFound(err) {
			return Artifact{}, ErrNotFound
		}
		return Artifact{}, fmt.Errorf("artifact transition: %w", err)
	}
	return a, nil
}

// SetArtifactSubmitted moves the working copy to pending review and records
// the submitter, the scanner findings, and the digest of those EXACT
// findings (migration 00028's scan_findings_digest). digest is the caller's
// govern.Digest(findings), this package stays govern-free (see
// Artifact.ScanFindings' own doc comment) and never computes it, only
// persists what it is given. Writing both columns in this ONE statement is
// what makes them unable to disagree: there is no second UPDATE that could
// run against a since-changed row or fail independently and leave a findings
// set with no matching digest (or vice versa).
//
// NULLIF stores an empty digest as NULL, matching
// Artifact.ScanFindingsDigest's ""-means-NULL contract (govern.Digest of an
// empty findings set returns "", the same convention scan_findings_digest
// uses for "no digest recorded").
//
// reject_reason is cleared (a resubmit supersedes a prior rejection).
//
// THE THREE findings_ack_* COLUMNS ARE CLEARED TOO, in this same statement,
// and that is a governance requirement rather than tidiness. A submission
// opens a new review cycle: this statement is already rewriting BOTH of the
// facts an acknowledgment is about, WHO put the content up (submitted_by) and
// WHAT the scanner said about it (scan_findings/_digest), so an
// acknowledgment recorded before it ran describes neither. Without the clear,
// a reject-then-resubmit by a DIFFERENT person carries the first submitter's
// acknowledgment across, and because the deterministic scanner is a pure
// function of content, resubmitting the content unchanged reproduces the same
// digest and every digest-based check downstream still matches: approve's
// author-acknowledgment gate passes, and the approval is recorded against an
// acknowledgment the actual submitter never made. Reproduced end to end by
// TestApproveRefusedWhenTheResubmitterNeverAcknowledged (internal/api).
//
// Clearing HERE rather than at approve time is deliberate. Approve is one
// reader of these columns; toArtifactDTO is another, and it compares digests
// with no notion of who submitted, so a check confined to approve would leave
// the Review queue showing "acknowledged by alice" on bob's submission while
// approve refused it. Fixing the row means every reader is right at once.
// It is the same reasoning WithdrawArtifact already applies for the same
// columns, and the same ONE-statement reasoning as the digest above: a
// separate clearing UPDATE could fail on its own and leave a fresh digest
// beside a foreign acknowledgment, which is precisely the state being closed.
//
// Consequence worth knowing, because it makes a whole class of row
// unreachable: after this, findings_ack_digest can only ever be either empty
// or equal to the current scan_findings_digest, since the only writer of a
// non-empty value (SetArtifactFindingsAcknowledged, called under a row lock
// by a handler that checks caller == submitted_by and digest ==
// scan_findings_digest) runs after this statement and against its values. The
// digest comparisons in handleApproveArtifact and toArtifactDTO are therefore
// defense in depth, not dead code, and the tests that exercise a genuinely
// mismatched pair now build it through this package rather than through the
// handlers, which can no longer produce one.
func (s *Store) SetArtifactSubmitted(ctx context.Context, tenantID, id, submitter string, findings []byte, digest string) (Artifact, error) {
	return s.transition(ctx, `UPDATE artifact SET approval_state='pending',
		submitted_by=$3, submitted_at=now(), reject_reason=NULL, scan_findings=$4,
		scan_findings_digest=NULLIF($5,''),
		findings_ack_digest=NULL, findings_ack_by=NULL, findings_ack_at=NULL,
		updated_at=now()
		WHERE tenant_id=$1 AND id=$2 RETURNING `+artifactCols, tenantID, id, submitter, findings, digest)
}

// SetArtifactApproved copies the working payload into the approved snapshot,
// records the approver, and appends an immutable 'approval' revision. MUST run
// inside a tx (all callers wrap it in auditedTx/InTx) so the snapshot write and
// the revision append are atomic. keep is insertRevision's prune cap (<=0 =
// unlimited, no pruning); pruned is the count of revisions the prune removed.
func (s *Store) SetArtifactApproved(ctx context.Context, tenantID, id, approver string, keep int) (a Artifact, pruned int64, err error) {
	a, err = s.transition(ctx, `UPDATE artifact SET approval_state='approved',
		approved_content=content, approved_memory_seed=memory_seed, approved_memory_scope=memory_scope,
		approved_type=type, approved_name=name, approved_visibility=visibility,
		approved_target_tags=target_tags, approved_rule_scope=rule_scope,
		approved_by=$3, approved_at=now(), updated_at=now()
		WHERE tenant_id=$1 AND id=$2 RETURNING `+artifactCols, tenantID, id, approver)
	if err != nil {
		return Artifact{}, 0, err
	}
	// The revision records the identity that was just frozen, read back out of
	// the approved_* columns rather than the live ones. They hold the same
	// values here by construction (the UPDATE above copied them one statement
	// ago), and reading the snapshot side keeps the revision a record of what
	// was approved rather than of what the row happens to say now.
	pruned, err = s.insertRevision(ctx, tenantID, id, a.Content, a.MemorySeed, a.MemoryScope,
		a.ApprovedType, a.ApprovedName, a.ApprovedVisibility, a.ApprovedTargetTags, a.ApprovedRuleScope, "approval", nil, approver, keep)
	if err != nil {
		return Artifact{}, 0, err
	}
	return a, pruned, nil
}

// SetArtifactRejected records a rejection reason; the working copy is preserved
// so the author can edit and resubmit.
func (s *Store) SetArtifactRejected(ctx context.Context, tenantID, id, reason string) (Artifact, error) {
	return s.transition(ctx, `UPDATE artifact SET approval_state='rejected',
		reject_reason=$3, updated_at=now()
		WHERE tenant_id=$1 AND id=$2 RETURNING `+artifactCols, tenantID, id, reason)
}

// WithdrawArtifact clears the approved snapshot (pulling the artifact from
// distribution) and returns the working copy to a clean draft. Unilateral:
// removing content is always fail-safe, so no approval is required. It also
// nulls the review-cycle fields (reject_reason, submitted_by/at, scan_findings)
// so a withdrawn artifact is a true draft — it must not serve a stale
// rejectReason (or submitter/findings) from a prior review round in its DTO or
// its withdraw audit event.
func (s *Store) WithdrawArtifact(ctx context.Context, tenantID, id string) (Artifact, error) {
	// scan_findings is NOT NULL DEFAULT '[]' — reset it to the empty-array default
	// (the same state a freshly-created draft has), not NULL.
	// approved_type/name/visibility go with the snapshot, not merely for
	// tidiness: artifact_approved_identity_complete (00016) rejects an identity
	// left behind by a cleared snapshot, so forgetting one here is a constraint
	// violation rather than a row that silently holds a stale distributed name.
	//
	// scan_findings_digest and the three findings_ack_* columns (migration
	// 00028) are reset to NULL for the same reason findings/submitted_by/
	// submitted_at are: they belong to a prior review round, and "a
	// withdrawn artifact is a true draft" applies to the acknowledgment of
	// findings exactly as it applies to the findings themselves. Leaving a
	// digest or an acknowledgment behind here would let a fresh submission
	// find a foreign digest sitting in the row before the new scan ever ran.
	return s.transition(ctx, `UPDATE artifact SET approval_state='draft',
		approved_content=NULL, approved_memory_seed=NULL, approved_memory_scope=NULL,
		approved_type=NULL, approved_name=NULL, approved_visibility=NULL,
		approved_target_tags=NULL, approved_rule_scope=NULL,
		approved_by=NULL, approved_at=NULL,
		reject_reason=NULL, submitted_by=NULL, submitted_at=NULL, scan_findings='[]'::jsonb,
		scan_findings_digest=NULL, findings_ack_digest=NULL, findings_ack_by=NULL, findings_ack_at=NULL,
		updated_at=now()
		WHERE tenant_id=$1 AND id=$2 RETURNING `+artifactCols, tenantID, id)
}

// SetArtifactFindingsAcknowledged records the AUTHOR's acknowledgment of the
// findings identified by digest: who acknowledged them and when, alongside
// the digest itself so a later re-scan (which computes a different digest)
// leaves this acknowledgment describing findings that no longer match rather
// than silently carrying forward. The caller is responsible for having
// already checked that digest matches the artifact's CURRENT
// scan_findings_digest (task 4's endpoint, under the row lock
// GetArtifactForUpdate already takes). This function itself performs no
// such comparison, only the tenant-scoped write.
func (s *Store) SetArtifactFindingsAcknowledged(ctx context.Context, tenantID, id, actor, digest string) (Artifact, error) {
	return s.transition(ctx, `UPDATE artifact SET
		findings_ack_digest=$3, findings_ack_by=$4, findings_ack_at=now(), updated_at=now()
		WHERE tenant_id=$1 AND id=$2 RETURNING `+artifactCols, tenantID, id, digest, actor)
}

// ClearArtifactFindingsAcknowledgment was DELETED, 2026-08-28, rather than
// wired: it had zero non-test callers, and its own doc comment claimed it was
// "what makes a resubmit's re-scan able to invalidate a stale
// acknowledgment", which nothing did, because nothing called it. The clearing
// it described now happens inside SetArtifactSubmitted's single UPDATE (see
// that function's comment), which is where it has to be: a separate statement
// can fail on its own and leave a fresh digest beside a foreign
// acknowledgment. This note is here so the next reader does not reintroduce
// the standalone version looking for the caller it never had.

func (s *Store) DeleteArtifact(ctx context.Context, tenantID, id string) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM artifact WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	if err != nil {
		if idCastNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("delete artifact: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PublishState is the per-tenant marketplace publish status (singleton row).
type PublishState struct {
	TenantID      string
	LastAttemptAt *time.Time
	LastSuccessAt *time.Time
	LastCommit    string
	LastError     string
}

func (s *Store) GetPublishState(ctx context.Context, tenantID string) (PublishState, error) {
	var ps PublishState
	err := s.db.QueryRow(ctx,
		`SELECT tenant_id::text, last_attempt_at, last_success_at, last_commit, last_error
		 FROM publish_state WHERE tenant_id=$1`, tenantID).
		Scan(&ps.TenantID, &ps.LastAttemptAt, &ps.LastSuccessAt, &ps.LastCommit, &ps.LastError)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PublishState{TenantID: tenantID}, nil
		}
		return PublishState{}, fmt.Errorf("get publish_state: %w", err)
	}
	return ps, nil
}

// RecordPublishSuccess records a successful publish: the target now holds commit.
// It advances last_attempt_at and last_success_at, and clears last_error.
func (s *Store) RecordPublishSuccess(ctx context.Context, tenantID string, at time.Time, commit string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO publish_state (tenant_id, last_attempt_at, last_success_at, last_commit, last_error)
		VALUES ($1,$2,$2,$3,'')
		ON CONFLICT (tenant_id) DO UPDATE SET
			last_attempt_at=EXCLUDED.last_attempt_at,
			last_success_at=EXCLUDED.last_success_at,
			last_commit=EXCLUDED.last_commit,
			last_error=''`,
		tenantID, at, commit)
	if err != nil {
		return fmt.Errorf("record publish success: %w", err)
	}
	return nil
}

// RecordPublishFailure records a failed publish attempt. The statement deliberately
// does NOT name last_commit or last_success_at: the last good publish must survive a
// failure, and making that a property of the SQL means no caller can forget it. On a
// first-ever failure the column defaults (empty string / NULL) correctly report "no
// publish yet".
func (s *Store) RecordPublishFailure(ctx context.Context, tenantID string, at time.Time, errMsg string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO publish_state (tenant_id, last_attempt_at, last_error)
		VALUES ($1,$2,$3)
		ON CONFLICT (tenant_id) DO UPDATE SET
			last_attempt_at=EXCLUDED.last_attempt_at,
			last_error=EXCLUDED.last_error`,
		tenantID, at, errMsg)
	if err != nil {
		return fmt.Errorf("record publish failure: %w", err)
	}
	return nil
}
