package store

import (
	"context"
	"fmt"
	"testing"
)

// revisionRow is the minimal shape these tests need to verify insertRevision's
// write directly against the artifact_revision table, WITHOUT going through
// ListArtifactRevisions — that reader lives in artifact_revision.ee.go
// (docs/specs/2026-08-19-orbeat-community-repo-generation-design.md), and a
// Community-tree test must not depend on code that tree does not ship, even
// though insertRevision itself (called from the shared SetArtifactApproved)
// does ship there.
type revisionRow struct {
	RevisionNum int
	Content     string
}

// queryRevisionsDesc reads an artifact's revision_num/content directly,
// newest-first — the same order ListArtifactRevisions returns, so the
// surviving-set assertions below read the same way they did before the split.
func queryRevisionsDesc(t *testing.T, st *Store, artifactID string) []revisionRow {
	t.Helper()
	rows, err := st.db.Query(context.Background(),
		`SELECT revision_num, content FROM artifact_revision WHERE artifact_id=$1 ORDER BY revision_num DESC`, artifactID)
	if err != nil {
		t.Fatalf("query revisions: %v", err)
	}
	defer rows.Close()
	var out []revisionRow
	for rows.Next() {
		var r revisionRow
		if err := rows.Scan(&r.RevisionNum, &r.Content); err != nil {
			t.Fatalf("scan revision: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestInsertRevisionPruneKeepsNewestContiguousSuffix is Task 2's decisive
// gate (spec §8): after pruning to keep=3 across 5 sequential inserts, the
// SURVIVING SET must be exactly {3,4,5} — a contiguous suffix ending at
// MAX(revision_num) — not merely 3 rows of any shape. A count-only assertion
// would still pass on a DELETE that kept the OLDEST 3, or one that kept 4.
//
// Pruning runs after every insert, so only once more than `keep` rows exist
// does a call actually delete anything: rows 1-3 insert with nothing to
// prune (pruned=0 each), row 4 removes revision 1 (pruned=1), row 5 removes
// revision 2 (pruned=1). Each step's returned count is asserted, which is
// also this test's coverage of "the returned count matches the rows actually
// removed" (the last gate in the plan's table) — a wrong count would show up
// immediately as a wrong-length surviving set once accumulated across steps.
func TestInsertRevisionPruneKeepsNewestContiguousSuffix(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	a, err := st.CreateArtifact(ctx, Artifact{TenantID: tn.ID, Type: "skill", Name: "prune-suffix", Content: "seed", Visibility: "org"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	wantPruned := []int64{0, 0, 0, 1, 1}
	for i := 1; i <= 5; i++ {
		pruned, err := st.insertRevision(ctx, tn.ID, a.ID, fmt.Sprintf("v%d", i), "", "", "approval", nil, "bob", 3)
		if err != nil {
			t.Fatalf("insertRevision %d: %v", i, err)
		}
		if pruned != wantPruned[i-1] {
			t.Fatalf("insert %d: pruned = %d, want %d", i, pruned, wantPruned[i-1])
		}
	}

	revs := queryRevisionsDesc(t, st, a.ID)
	if len(revs) != 3 {
		t.Fatalf("want 3 surviving revisions, got %d: %+v", len(revs), revs)
	}
	// Newest-first: exactly {5,4,3}, each with its own content — proves the
	// SET, not just the count.
	wantNums := []int{5, 4, 3}
	for i, r := range revs {
		if r.RevisionNum != wantNums[i] || r.Content != fmt.Sprintf("v%d", wantNums[i]) {
			t.Fatalf("revs[%d] = {num:%d content:%q}, want {num:%d content:%q}",
				i, r.RevisionNum, r.Content, wantNums[i], fmt.Sprintf("v%d", wantNums[i]))
		}
	}
}

// TestInsertRevisionPruneLeavesOtherArtifactsUntouched is spec §7's gate: the
// DELETE must be scoped by artifact_id. A second artifact's revisions, built
// up past the same keep cap, must be untouched by the first artifact's prune.
func TestInsertRevisionPruneLeavesOtherArtifactsUntouched(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	a, err := st.CreateArtifact(ctx, Artifact{TenantID: tn.ID, Type: "skill", Name: "prune-a", Content: "seed", Visibility: "org"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := st.CreateArtifact(ctx, Artifact{TenantID: tn.ID, Type: "skill", Name: "prune-b", Content: "seed", Visibility: "org"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	// b accumulates 4 unpruned revisions (keep=0) BEFORE a's pruning insert
	// runs, so a's DELETE has every chance to leak across artifact_id if it
	// is not scoped.
	for i := 1; i <= 4; i++ {
		if _, err := st.insertRevision(ctx, tn.ID, b.ID, fmt.Sprintf("b%d", i), "", "", "approval", nil, "bob", 0); err != nil {
			t.Fatalf("insertRevision b%d: %v", i, err)
		}
	}
	for i := 1; i <= 5; i++ {
		if _, err := st.insertRevision(ctx, tn.ID, a.ID, fmt.Sprintf("a%d", i), "", "", "approval", nil, "bob", 2); err != nil {
			t.Fatalf("insertRevision a%d: %v", i, err)
		}
	}

	aRevs := queryRevisionsDesc(t, st, a.ID)
	if len(aRevs) != 2 {
		t.Fatalf("a: want 2 surviving revisions (keep=2), got %d: %+v", len(aRevs), aRevs)
	}

	bRevs := queryRevisionsDesc(t, st, b.ID)
	if len(bRevs) != 4 {
		t.Fatalf("b: want all 4 revisions untouched by a's prune, got %d: %+v", len(bRevs), bRevs)
	}
}

// TestSetArtifactApprovedThreadsKeepToInsertRevision is Task 3's own gate:
// SetArtifactApproved must actually forward its keep parameter to
// insertRevision and surface the pruned count, not silently drop it (the
// literal 0 both call sites passed as a Task-2 compile fix is exactly the
// value this test would fail to distinguish from a correctly-wired keep=3 on
// its first three approvals — the 4th and 5th are where a dropped parameter
// would show as a returned pruned count of 0 instead of 1).
func TestSetArtifactApprovedThreadsKeepToInsertRevision(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	a, err := st.CreateArtifact(ctx, Artifact{TenantID: tn.ID, Type: "skill", Name: "approve-prune", Content: "FIXED", Visibility: "org"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	wantPruned := []int64{0, 0, 0, 1, 1}
	var lastApproved Artifact
	for i := 1; i <= 5; i++ {
		var pruned int64
		if err := st.InTx(ctx, func(tx *Store) error {
			if _, e := tx.GetArtifactForUpdate(ctx, tn.ID, a.ID); e != nil {
				return e
			}
			var e error
			lastApproved, pruned, e = tx.SetArtifactApproved(ctx, tn.ID, a.ID, "bob", 3)
			return e
		}); err != nil {
			t.Fatalf("approve %d: %v", i, err)
		}
		if pruned != wantPruned[i-1] {
			t.Fatalf("approve %d: pruned = %d, want %d — SetArtifactApproved is not forwarding keep to insertRevision", i, pruned, wantPruned[i-1])
		}
	}

	revs := queryRevisionsDesc(t, st, a.ID)
	if len(revs) != 3 {
		t.Fatalf("want 3 surviving revisions, got %d: %+v", len(revs), revs)
	}
	if revs[0].RevisionNum != 5 {
		t.Fatalf("newest surviving revision must be #5: %+v", revs[0])
	}
	if revs[0].Content != lastApproved.ApprovedContent {
		t.Fatalf("max revision content %q must equal artifact.approved_content %q", revs[0].Content, lastApproved.ApprovedContent)
	}
}

// TestInsertRevisionKeepZeroOrNegativePrunesNothing pins keep<=0 as the "off"
// sentinel (spec §5, default): no DELETE runs at all, regardless of how many
// revisions already exist.
func TestInsertRevisionKeepZeroOrNegativePrunesNothing(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	for _, keep := range []int{0, -1, -100} {
		a, err := st.CreateArtifact(ctx, Artifact{TenantID: tn.ID, Type: "skill", Name: fmt.Sprintf("prune-off-%d", keep), Content: "seed", Visibility: "org"})
		if err != nil {
			t.Fatalf("create (keep=%d): %v", keep, err)
		}
		for i := 1; i <= 5; i++ {
			pruned, err := st.insertRevision(ctx, tn.ID, a.ID, fmt.Sprintf("v%d", i), "", "", "approval", nil, "bob", keep)
			if err != nil {
				t.Fatalf("insertRevision (keep=%d) %d: %v", keep, i, err)
			}
			if pruned != 0 {
				t.Fatalf("insertRevision (keep=%d) %d: pruned = %d, want 0", keep, i, pruned)
			}
		}
		revs := queryRevisionsDesc(t, st, a.ID)
		if len(revs) != 5 {
			t.Fatalf("keep=%d: want all 5 revisions kept, got %d", keep, len(revs))
		}
	}
}
