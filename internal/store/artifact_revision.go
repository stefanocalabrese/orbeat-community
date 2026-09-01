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
// artType, name and visibility are the approved IDENTITY this revision froze
// (migration 00016), which is what makes a revision the COMPLETE approved
// state: without them a rollback would restore old content under whatever the
// artifact is called now, the exact desync the snapshot exists to prevent.
// They are written through NULLIF so an empty string lands as NULL, the same
// value a revision approved before 00016 carries and which rollback reads as
// "restore the content, leave the approved identity where it is".
func (s *Store) insertRevision(ctx context.Context, tenantID, artifactID, content, seed, scope, artType, name, visibility string, targetTags []string, ruleScope, source string, restoredFrom *int, actor string, keep int) (pruned int64, err error) {
	_, err = s.db.Exec(ctx, `
		INSERT INTO artifact_revision
			(tenant_id, artifact_id, revision_num, content, memory_seed, memory_scope,
			 type, name, visibility, target_tags, rule_scope, source, restored_from_num, approved_by)
		VALUES ($1, $2,
			COALESCE((SELECT MAX(revision_num) FROM artifact_revision WHERE artifact_id=$2), 0) + 1,
			$3, NULLIF($4,''), NULLIF($5,''),
			NULLIF($6,''), NULLIF($7,''), NULLIF($8,''), NULLIF($9,'{}'::text[]), NULLIF($10,''), $11, $12, $13)`,
		tenantID, artifactID, content, seed, scope, artType, name, visibility, targetTags, ruleScope, source, restoredFrom, actor)
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

// ArtifactRevisionKey names one revision of one artifact. It is both the batch
// key ListArtifactRevisionPayloads is asked with and the key its result map is
// built on, and the second half is the load-bearing one: a result keyed on
// artifact_id alone collapses two pinned revisions of the same artifact onto a
// single entry, and one of the two developers then receives bytes that were
// never at her pin.
type ArtifactRevisionKey struct {
	ArtifactID  string
	RevisionNum int
}

// ArtifactRevisionPayload is one revision's frozen distributable state: the
// bytes that were approved, and the identity they were approved under.
//
// THERE IS NO Visibility FIELD, and adding one would be a security regression
// rather than a missing feature (pinning design sec 5.3). Entitlement and
// visibility are always evaluated against the LIVE artifact and
// artifact_entitlement rows, so a revoked grant or a flip to org visibility
// ends distribution on the next sync no matter what any pin says. A caller who
// found visibility already sitting in this struct would have no reason to
// think twice before filtering on it, which is precisely how that boundary
// gets crossed by somebody who never read this comment.
type ArtifactRevisionPayload struct {
	Content     string
	MemorySeed  string
	MemoryScope string

	// Type and Name are the identity this revision FROZE (migration 00016),
	// not the artifact's current one, so a pinned developer keeps the file at
	// the path the artifact had when that revision was approved while an
	// unpinned one moves when an admin renames it. orbeat-sync derives every
	// local path from type plus name (fileBackedTypes,
	// internal/syncclient/reconcile.go), so serving revision 3's content under
	// revision 9's name would put content on disk that was never approved in
	// that shape.
	//
	// They fall back to the artifact's approved identity on a revision written
	// before 00016, which recorded none. See the COALESCE in the query below.
	Type string
	Name string
}

// ListArtifactRevisionPayloads reads the frozen payload and identity of every
// revision named in keys, in ONE query, keyed back on the
// (artifact_id, revision_num) pair it was asked for.
//
// A key that names no surviving revision, or one belonging to another tenant,
// is simply ABSENT from the result rather than an error. That is the ordinary
// case, not a degenerate one: a pin outlives the revision it names as soon as
// ORBEAT_ARTIFACT_REVISION_KEEP prunes it, and the caller resolves that by
// clamping into the window that still exists (pinning design sec 4.2).
//
// ONE query for the whole batch, never one per artifact. The entitled set runs
// to tens of rows and this sits on the sync read path, so a per-artifact round
// trip here is the shape v1.18.0 removed from the resolver.
//
// SHARED, not artifact_revision.ee.go, and the deciding fact is the CALLER,
// not the feature. Pinning is Enterprise only, yet handleSyncArtifacts
// (internal/api/sync.go) is a shared handler that gates the whole pin path on
// s.pinning at runtime, so a generated Community tree compiles this call site
// and would fail to build if this function were dropped from it. That is
// insertRevision's own argument above pointing the same way. The alternative,
// an .ee.go definition plus a .community.go twin returning an empty map,
// doubles an extension point for a function no Community path can reach and
// hands the twin a signature to keep in step forever.
//
// The result is a map, so no caller can depend on row order, and every row
// carries its own key: content and identity are read out of one revision row
// and filed under that same row's pair. A batch read is where a pairing bug
// lives, and this is the shape that has none to find.
//
// IDENTITY COMES FROM THE REVISION, falling back to the artifact's APPROVED
// identity when the revision recorded none. That fallback is not a second
// implementation of RollbackArtifact's: it is the same COALESCE(approved_type,
// type) / COALESCE(approved_name, name) chain that function reads at
// artifact_revision.ee.go, applied under the same all-or-none guard, and
// TestPinnedPayloadIdentityAgreesWithRollback drives both functions over one
// identity-less fixture and asserts they resolve the same pair. Two fallbacks
// that are merely written to agree drift; this one is checked.
//
// The guard is a PAIR here and a triple there only because sec 5.3 forbids
// reading visibility at all. It is the same partition either way: insertRevision
// writes the three identity columns through NULLIF from one row every time, so
// all three are NULL together or none is, and a half-recorded revision would be
// corruption rather than a case to branch on.
//
// tenant_id=$1 is defense-in-depth, not a live-defect fix, mirroring
// artifactRevisionPageSQL's own tenant_id=$1 in artifact_revision.ee.go:
// artifact_id is a globally unique uuid, so no cross-tenant test can ever fail
// for its absence. It follows the v1.16.0 precedent of tenant-scoping store
// statements directly in SQL even when a caller already checked tenancy, and
// the honest record of that is this sentence rather than a test that would
// pass with the clause deleted.
//
// A repeated key costs a duplicate row and nothing else: the second write
// lands on the same map entry with the same value. That is why this has no
// dedupe step, unlike ReplaceDeployments, whose ON CONFLICT DO UPDATE raises
// 21000 when its source names one conflict key twice.
//
// Malformed ids are the caller's to reject: t.artifact_id::uuid raises 22P02
// here, unmapped, exactly as ReplaceDeployments leaves it. The pin parser
// validates each id before a key reaches this function.
func (s *Store) ListArtifactRevisionPayloads(ctx context.Context, tenantID string, keys []ArtifactRevisionKey) (map[ArtifactRevisionKey]ArtifactRevisionPayload, error) {
	out := make(map[ArtifactRevisionKey]ArtifactRevisionPayload, len(keys))
	if len(keys) == 0 {
		return out, nil
	}

	ids := make([]string, len(keys))
	nums := make([]int32, len(keys))
	for i, k := range keys {
		ids[i] = k.ArtifactID
		nums[i] = int32(k.RevisionNum)
	}

	// Every predicate names its table on both sides. artifact_revision carries
	// an id of its own, so a bare column in a join or a subquery predicate can
	// resolve to the wrong relation and produce a plausible wrong answer with
	// no error at all, which is the trap distArtifactCols' comment records for
	// its two aggregates.
	rows, err := s.db.Query(ctx, `
		WITH input AS (
			SELECT t.artifact_id::uuid AS artifact_id, t.revision_num
			FROM unnest($2::text[], $3::int[]) AS t(artifact_id, revision_num)
		)
		SELECT r.artifact_id::text, r.revision_num,
		       r.content, r.memory_seed, r.memory_scope, r.type, r.name,
		       COALESCE(a.approved_type, a.type), COALESCE(a.approved_name, a.name)
		FROM artifact_revision r
		JOIN artifact a ON a.id = r.artifact_id
		JOIN input ON input.artifact_id = r.artifact_id
		          AND input.revision_num = r.revision_num
		WHERE a.tenant_id = $1`, tenantID, ids, nums)
	if err != nil {
		return nil, fmt.Errorf("list artifact revision payloads: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key ArtifactRevisionKey
		var p ArtifactRevisionPayload
		// seed and scope are nullable on artifact_revision (00007); identity is
		// nullable for a pre-00016 revision. Both fold to "" the way
		// queryArtifactRevisions already folds them.
		var seed, scope, revType, revName *string
		var fallbackType, fallbackName string
		if err := rows.Scan(&key.ArtifactID, &key.RevisionNum,
			&p.Content, &seed, &scope, &revType, &revName,
			&fallbackType, &fallbackName); err != nil {
			return nil, fmt.Errorf("scan artifact revision payload: %w", err)
		}
		if seed != nil {
			p.MemorySeed = *seed
		}
		if scope != nil {
			p.MemoryScope = *scope
		}
		p.Type, p.Name = fallbackType, fallbackName
		if revType != nil && revName != nil {
			p.Type, p.Name = *revType, *revName
		}
		out[key] = p
	}
	return out, rows.Err()
}

// MaxArtifactRevisionNum returns MAX(revision_num) over a tenant-scoped
// artifact's revision chain, or 0 when the chain is empty.
//
// 0 IS "NO REVISION EXISTS", NOT AN ERROR, and it is unambiguous because
// insertRevision numbers from 1. An artifact with an empty chain is reachable
// only by direct SQL through any API path today (00007's Up grandfathers every
// approved artifact as revision 1 and every approval appends one), which is
// exactly why the caller must still handle it: handleSetArtifactMinRevision
// rejects any floor above 0 on such a row rather than writing a floor nobody
// can ever satisfy.
//
// CALL IT ONLY WHILE HOLDING THE ARTIFACT'S ROW LOCK. Both writers of this
// chain, SetArtifactApproved and RollbackArtifact, reach insertRevision inside
// a transaction that already holds artifact's FOR UPDATE lock, and
// insertRevision's prune DELETE runs there too, so a caller that took
// GetArtifactForUpdate first reads a MAX that cannot move under it. A caller
// that did not is reading a number that is stale the instant it returns.
//
// Tenant-scoped through the artifact rather than by trusting the id, matching
// RollbackArtifact's own target-revision read. artifact_id is a globally
// unique uuid, so no cross-tenant test can fail for the clause's absence; it
// is the same defense-in-depth ListArtifactRevisionPayloads documents above,
// recorded in a sentence rather than in a test that would pass either way.
//
// SHARED rather than artifact_revision.ee.go, and the rule is the file split's
// own: internal/store carries no .community.go twin at all, and its .ee.go
// files mark FEATURES the generator drops whole (revision history, rollback,
// the deployment registry), not the edition of whoever calls them. This is the
// same aggregate distArtifactCols computes on every distribution read in both
// editions, so putting it behind the Enterprise boundary would file a generic
// number under a feature it is not part of.
func (s *Store) MaxArtifactRevisionNum(ctx context.Context, tenantID, artifactID string) (int, error) {
	var maxNum int
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE((SELECT MAX(revision_num) FROM artifact_revision
		                 WHERE artifact_revision.artifact_id = artifact.id), 0)
		FROM artifact WHERE artifact.tenant_id=$1 AND artifact.id=$2`,
		tenantID, artifactID).Scan(&maxNum)
	if err != nil {
		if idCastNotFound(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("max artifact revision num: %w", err)
	}
	return maxNum, nil
}
