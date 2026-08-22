package store

import (
	"context"
	"fmt"
)

// insertRevision appends a revision, numbering it MAX(revision_num)+1 for the
// artifact, then — when keep > 0 — prunes the artifact's revisions down to
// the newest keep, in the SAME transaction. keep <= 0 performs no DELETE and
// returns 0 (off by default; spec §5, docs/specs/2026-08-19-orbeat-revision-
// pruning-design.md). Callers MUST run inside a tx that holds the artifact
// row lock (GetArtifactForUpdate) so the MAX+1 read-modify is race-free; the
// UNIQUE(artifact_id, revision_num) constraint is the backstop.
//
// Kept OUTSIDE the .ee build boundary (docs/specs/2026-08-19-orbeat-community-
// repo-generation-design.md): SetArtifactApproved (internal/store/artifact.go,
// shared) calls this on every approval, including the auto-approve path
// Community will use once that slice lands, so a Community build must still
// compile and run it even though it has no revision-history *reader* — this
// is an append-only write with no exposed history to browse or roll back to.
// The read side (ListArtifactRevisions/Page, RollbackArtifact, and their
// helpers) lives in artifact_revision.ee.go: nothing outside the Enterprise
// artifact-review handlers ever reads what this appends.
//
// Prune runs AFTER the insert. A keep-newest-N prune never deletes the row
// holding MAX(revision_num) for any N >= 1, so MAX is invariant under the
// prune and a prune-then-insert order would number identically — ordering is
// not about avoiding a revision_num collision. The real reason is arithmetic:
// inserting first lets the DELETE below retain exactly `keep` rows in one
// statement, with no "leave room for the row about to arrive" adjustment to
// get wrong.
//
// The insert and the prune are deliberately two statements, not one
// data-modifying CTE: every sub-statement of a CTE runs against the SAME
// snapshot, so a DELETE folded into the INSERT's CTE would not see the row
// the INSERT just wrote and would retain keep+1.
func (s *Store) insertRevision(ctx context.Context, tenantID, artifactID, content, seed, scope, source string, restoredFrom *int, actor string, keep int) (pruned int64, err error) {
	_, err = s.db.Exec(ctx, `
		INSERT INTO artifact_revision
			(tenant_id, artifact_id, revision_num, content, memory_seed, memory_scope, source, restored_from_num, approved_by)
		VALUES ($1, $2,
			COALESCE((SELECT MAX(revision_num) FROM artifact_revision WHERE artifact_id=$2), 0) + 1,
			$3, NULLIF($4,''), NULLIF($5,''), $6, $7, $8)`,
		tenantID, artifactID, content, seed, scope, source, restoredFrom, actor)
	if err != nil {
		return 0, fmt.Errorf("insert artifact revision: %w", err)
	}
	if keep <= 0 {
		return 0, nil
	}
	// tenant_id=$1 is defense-in-depth, not a live-defect fix, mirroring
	// artifactRevisionPageSQL's own tenant_id=$1 in artifact_revision.ee.go:
	// artifact_id is a globally unique uuid, so no cross-tenant test can ever
	// fail for its absence — it follows the v1.16.0 precedent of
	// tenant-scoping store statements directly in SQL even when a caller
	// already checked tenancy.
	ct, err := s.db.Exec(ctx, `
		DELETE FROM artifact_revision
		WHERE tenant_id=$1 AND artifact_id=$2
		  AND revision_num NOT IN (
			SELECT revision_num FROM artifact_revision
			WHERE artifact_id=$2
			ORDER BY artifact_revision.revision_num DESC
			LIMIT $3)`,
		tenantID, artifactID, keep)
	if err != nil {
		return 0, fmt.Errorf("prune artifact revisions: %w", err)
	}
	return ct.RowsAffected(), nil
}
