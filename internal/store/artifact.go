package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
	SubmittedBy         string
	SubmittedAt         *time.Time
	ApprovedBy          string
	ApprovedAt          *time.Time
	RejectReason        string
	ScanFindings        json.RawMessage // jsonb array of govern.Finding; store stays govern-free

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
}

const artifactCols = `id::text, tenant_id::text, type, name, description, content,
	memory_scope, memory_seed, version, visibility,
	approval_state, approved_content, approved_memory_seed, approved_memory_scope,
	submitted_by, submitted_at, approved_by, approved_at, reject_reason, scan_findings,
	approved_content IS NOT NULL AS has_approved, row_version`

// artifactSlimCols is artifactCols with the four heavy payload columns replaced
// by typed NULL / empty-string placeholders: content (<=64 KiB), memory_seed
// (<=16 KiB), approved_content (<=64 KiB) and approved_memory_seed (<=16 KiB;
// caps per govern.MaxContentBytes / govern.MaxSeedBytes) — 64+16+64+16 =
// ~160 KiB per row, which a 100-row page would turn into ~16 MB.
//
// The column ORDER and COUNT must stay identical to artifactCols: both are
// scanned by scanArtifact. has_approved is computed from approved_content even
// though the column itself is not selected, so the flag survives the slimming.
const artifactSlimCols = `id::text, tenant_id::text, type, name, description, '' AS content,
	memory_scope, NULL::text AS memory_seed, version, visibility,
	approval_state, NULL::text AS approved_content, NULL::text AS approved_memory_seed, approved_memory_scope,
	submitted_by, submitted_at, approved_by, approved_at, reject_reason, scan_findings,
	approved_content IS NOT NULL AS has_approved, row_version`

func scanArtifact(row interface{ Scan(...any) error }) (Artifact, error) {
	var a Artifact
	var memScope, memSeed, appContent, appSeed, appScope, subBy, appBy, rejReason *string
	err := row.Scan(&a.ID, &a.TenantID, &a.Type, &a.Name, &a.Description,
		&a.Content, &memScope, &memSeed, &a.Version, &a.Visibility,
		&a.ApprovalState, &appContent, &appSeed, &appScope,
		&subBy, &a.SubmittedAt, &appBy, &a.ApprovedAt, &rejReason, &a.ScanFindings,
		&a.HasApproved, &a.RowVersion)
	if err != nil {
		return Artifact{}, err
	}
	for dst, src := range map[*string]*string{
		&a.MemoryScope: memScope, &a.MemorySeed: memSeed,
		&a.ApprovedContent: appContent, &a.ApprovedMemorySeed: appSeed,
		&a.ApprovedMemoryScope: appScope, &a.SubmittedBy: subBy,
		&a.ApprovedBy: appBy, &a.RejectReason: rejReason,
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
	return a, nil
}

func (s *Store) CreateArtifact(ctx context.Context, a Artifact) (Artifact, error) {
	row := s.db.QueryRow(ctx, `
		INSERT INTO artifact (tenant_id, type, name, description, content, memory_scope, memory_seed, version, visibility)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,COALESCE(NULLIF($9,''),'org'))
		RETURNING `+artifactCols,
		a.TenantID, a.Type, a.Name, a.Description, a.Content, a.MemoryScope, a.MemorySeed, a.Version, a.Visibility)
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

// artifactKeys is artifact's sort order (id appended by keysetTail).
var artifactKeys = []sortKey{{Col: "type", Cast: "text"}, {Col: "name", Cast: "text"}}

// ArtifactCursor is the keyset position just after a.
func ArtifactCursor(a Artifact) ListCursor {
	return ListCursor{Keys: []string{a.Type, a.Name}, ID: a.ID}
}

// ArtifactPageOpts narrows a paginated artifact list.
type ArtifactPageOpts struct {
	State          string      // "" = no approval-state filter
	Cursor         *ListCursor // nil = first page
	Limit          int         // <= 0 = no limit
	IncludeContent bool        // false = the heavy payload columns are omitted
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
	base := `SELECT ` + cols + ` FROM artifact
		WHERE tenant_id = $1 AND ($2::text IS NULL OR approval_state = $2)`
	tail, tailArgs, err := keysetTail("artifact", artifactKeys, false, o.Cursor, o.Limit, 2)
	if err != nil {
		return "", nil, err
	}
	var stateArg any
	if o.State != "" {
		stateArg = o.State
	}
	return base + tail, append([]any{tenantID, stateArg}, tailArgs...), nil
}

// ListArtifactsPage returns up to o.Limit artifacts for a tenant ordered
// (type, name, id), starting strictly after o.Cursor.
//
// The approval-state filter runs in SQL, not in a Go loop over the result: with
// the filter applied after LIMIT, a page would come back short — or empty —
// while more matches existed (spec §4.1). That is a correctness bug, not a
// performance one.
func (s *Store) ListArtifactsPage(ctx context.Context, tenantID string, o ArtifactPageOpts) ([]Artifact, error) {
	sql, args, err := artifactPageSQL(tenantID, o)
	if err != nil {
		return nil, fmt.Errorf("artifact page cursor: %w", err)
	}
	return s.queryArtifacts(ctx, sql, args...)
}

// distArtifactCols is the minimal projection distribution consumers need
// (marketplace renderer + sync DTO): the APPROVED snapshot aliased into the
// working field positions so downstream code is unchanged.
const distArtifactCols = `type, name, approved_content, approved_memory_scope, approved_memory_seed`

func scanDistArtifact(row interface{ Scan(...any) error }) (Artifact, error) {
	var a Artifact
	var scope, seed *string
	if err := row.Scan(&a.Type, &a.Name, &a.Content, &scope, &seed); err != nil {
		return Artifact{}, err
	}
	if scope != nil {
		a.MemoryScope = *scope
	}
	if seed != nil {
		a.MemorySeed = *seed
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

// ListActiveOrgArtifacts returns org-visibility artifacts that have an approved
// snapshot (the Channel-1 plugin input). Content/MemoryScope/MemorySeed carry the
// approved snapshot, not the editable working copy.
//
// The unqualified `ORDER BY type, name` below is the one remaining unqualified
// ORDER BY in this package, and it is safe today ONLY because distArtifactCols
// projects `type, name` uncast and unaliased — the output labels ARE the
// column names, so a bare name in ORDER BY resolves to the same thing either
// way (unlike the C3 class this package otherwise guards against, e.g.
// artifact_entitlement's former `role_id::text` projection). This is
// load-bearing on distArtifactCols staying uncast: if a future change adds a
// cast to either column in that projection, this ORDER BY must be
// table-qualified too, the same fix keysetTail applies everywhere else.
func (s *Store) ListActiveOrgArtifacts(ctx context.Context, tenantID string) ([]Artifact, error) {
	return s.queryDistArtifacts(ctx, `SELECT `+distArtifactCols+` FROM artifact
		WHERE tenant_id=$1 AND visibility='org' AND approved_content IS NOT NULL
		ORDER BY type, name`, tenantID)
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
		         approval_state='draft', updated_at=now()
		       WHERE tenant_id=$1 AND id=$2 AND row_version=$11
		       RETURNING 1
		     )
		SELECT (SELECT count(*) FROM cur), (SELECT count(*) FROM upd)`
	var existsCnt, updCnt int
	err := s.db.QueryRow(ctx, q,
		a.TenantID, a.ID, a.Type, a.Name, a.Description, a.Content, a.MemoryScope, a.MemorySeed, a.Version, a.Visibility, expected,
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

func (s *Store) transition(ctx context.Context, sql string, args ...any) (Artifact, error) {
	a, err := scanArtifact(s.db.QueryRow(ctx, sql, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Artifact{}, ErrNotFound
		}
		return Artifact{}, fmt.Errorf("artifact transition: %w", err)
	}
	return a, nil
}

// SetArtifactSubmitted moves the working copy to pending review and records the
// submitter + scanner findings. reject_reason is cleared (a resubmit supersedes
// a prior rejection).
func (s *Store) SetArtifactSubmitted(ctx context.Context, tenantID, id, submitter string, findings []byte) (Artifact, error) {
	return s.transition(ctx, `UPDATE artifact SET approval_state='pending',
		submitted_by=$3, submitted_at=now(), reject_reason=NULL, scan_findings=$4, updated_at=now()
		WHERE tenant_id=$1 AND id=$2 RETURNING `+artifactCols, tenantID, id, submitter, findings)
}

// SetArtifactApproved copies the working payload into the approved snapshot,
// records the approver, and appends an immutable 'approval' revision. MUST run
// inside a tx (all callers wrap it in auditedTx/InTx) so the snapshot write and
// the revision append are atomic. keep is insertRevision's prune cap (<=0 =
// unlimited, no pruning); pruned is the count of revisions the prune removed.
func (s *Store) SetArtifactApproved(ctx context.Context, tenantID, id, approver string, keep int) (a Artifact, pruned int64, err error) {
	a, err = s.transition(ctx, `UPDATE artifact SET approval_state='approved',
		approved_content=content, approved_memory_seed=memory_seed, approved_memory_scope=memory_scope,
		approved_by=$3, approved_at=now(), updated_at=now()
		WHERE tenant_id=$1 AND id=$2 RETURNING `+artifactCols, tenantID, id, approver)
	if err != nil {
		return Artifact{}, 0, err
	}
	pruned, err = s.insertRevision(ctx, tenantID, id, a.Content, a.MemorySeed, a.MemoryScope, "approval", nil, approver, keep)
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
	return s.transition(ctx, `UPDATE artifact SET approval_state='draft',
		approved_content=NULL, approved_memory_seed=NULL, approved_memory_scope=NULL,
		approved_by=NULL, approved_at=NULL,
		reject_reason=NULL, submitted_by=NULL, submitted_at=NULL, scan_findings='[]'::jsonb,
		updated_at=now()
		WHERE tenant_id=$1 AND id=$2 RETURNING `+artifactCols, tenantID, id)
}

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
