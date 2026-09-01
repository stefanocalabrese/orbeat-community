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
		pruned, err := st.insertRevision(ctx, tn.ID, a.ID, fmt.Sprintf("v%d", i), "", "", a.Type, a.Name, a.Visibility, nil, "", "approval", nil, "bob", 3)
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
		if _, err := st.insertRevision(ctx, tn.ID, b.ID, fmt.Sprintf("b%d", i), "", "", b.Type, b.Name, b.Visibility, nil, "", "approval", nil, "bob", 0); err != nil {
			t.Fatalf("insertRevision b%d: %v", i, err)
		}
	}
	for i := 1; i <= 5; i++ {
		if _, err := st.insertRevision(ctx, tn.ID, a.ID, fmt.Sprintf("a%d", i), "", "", a.Type, a.Name, a.Visibility, nil, "", "approval", nil, "bob", 2); err != nil {
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
			pruned, err := st.insertRevision(ctx, tn.ID, a.ID, fmt.Sprintf("v%d", i), "", "", a.Type, a.Name, a.Visibility, nil, "", "approval", nil, "bob", keep)
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

// approveInTx approves an artifact inside a tx (SetArtifactApproved appends a
// revision, so it must run transactionally, like every real caller does). It
// returns the post-approval row so callers that go on to UpdateArtifact can
// thread its fresh RowVersion through instead of an artifact-var snapshot
// that SetArtifactApproved's own row_version bump has since made stale.
//
// SHARED rather than in artifact_revision.ee_test.go, where it used to live:
// it touches only shared writers, and the payload gates below run in a
// generated Community tree, which drops that file entirely.
func approveInTx(t *testing.T, st *Store, tenantID, id, approver string) Artifact {
	t.Helper()
	var out Artifact
	if err := st.InTx(context.Background(), func(tx *Store) error {
		if _, e := tx.GetArtifactForUpdate(context.Background(), tenantID, id); e != nil {
			return e
		}
		var e error
		out, _, e = tx.SetArtifactApproved(context.Background(), tenantID, id, approver, 0)
		return e
	}); err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
	return out
}

// reviseAndApprove replaces an artifact's name, body and memory fields, then
// approves the result, returning the fresh row. The row_version is re-read from
// a, which every caller holds fresh out of the previous approval, because the
// 00013 trigger bumps it on approvals too.
func reviseAndApprove(t *testing.T, st *Store, tenantID string, a Artifact, name, content, seed, scope string) Artifact {
	t.Helper()
	a.Name, a.Content, a.MemorySeed, a.MemoryScope = name, content, seed, scope
	updated, err := st.UpdateArtifact(context.Background(), a, a.RowVersion)
	if err != nil {
		t.Fatalf("revise to %s: %v", name, err)
	}
	return approveInTx(t, st, tenantID, updated.ID, "bob")
}

// TestListArtifactRevisionPayloadsPairsEveryKeyWithItsOwnRevision is the
// batched read's decisive gate, and the fixture is the gate rather than the
// assertions: three keys, spanning TWO artifacts, two of them different
// revisions of the SAME artifact, every body and every memory field distinct.
//
// A fixture asking for one revision cannot see any of the three bugs a batch
// read is where you find. It cannot see a result keyed on artifact_id alone,
// which collapses the two alpha keys and serves one developer bytes that were
// never at her pin. It cannot see a join that pairs on artifact and ignores
// revision_num, which hands back rows nobody asked for. And it cannot see rows
// coming back in an order the caller did not expect, because with one row every
// order is the right one.
//
// It also pins the identity as FROZEN. alpha is on its third approved name by
// the time the read runs, so a payload reporting the artifact's CURRENT
// approved identity and one reporting the revision's own are two different
// strings here. They coincide on any fixture that never renames, which is why
// this one renames twice.
func TestListArtifactRevisionPayloadsPairsEveryKeyWithItsOwnRevision(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	alpha, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "alpha-one", Content: "ALPHA-BODY-1",
		MemorySeed: "ALPHA-SEED-1", MemoryScope: "project", Visibility: "org",
	})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	alpha = approveInTx(t, st, tn.ID, alpha.ID, "bob")
	alpha = reviseAndApprove(t, st, tn.ID, alpha, "alpha-two", "ALPHA-BODY-2", "ALPHA-SEED-2", "user")
	alpha = reviseAndApprove(t, st, tn.ID, alpha, "alpha-three", "ALPHA-BODY-3", "ALPHA-SEED-3", "local")

	// bravo is a skill, so it carries no seed and no scope: the NULL fold is
	// covered by the same call that covers the populated one.
	bravo, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "bravo-one", Content: "BRAVO-BODY-1", Visibility: "org",
	})
	if err != nil {
		t.Fatalf("create bravo: %v", err)
	}
	bravo = approveInTx(t, st, tn.ID, bravo.ID, "bob")
	bravo = reviseAndApprove(t, st, tn.ID, bravo, "bravo-two", "BRAVO-BODY-2", "", "")

	// PRECONDITION: both artifacts have moved on from the revisions the read
	// asks for. Without this the frozen-identity assertions below could pass on
	// an implementation that reads identity off the live artifact row.
	if alpha.ApprovedName != "alpha-three" || bravo.ApprovedName != "bravo-two" {
		t.Fatalf("precondition: want approved names alpha-three/bravo-two, got %q/%q",
			alpha.ApprovedName, bravo.ApprovedName)
	}

	// Deliberately out of revision order and interleaved across artifacts: the
	// result is a map, and nothing about it may depend on the order rows arrive
	// in or the order they were asked for.
	keys := []ArtifactRevisionKey{
		{ArtifactID: alpha.ID, RevisionNum: 2},
		{ArtifactID: bravo.ID, RevisionNum: 1},
		{ArtifactID: alpha.ID, RevisionNum: 1},
	}
	got, err := st.ListArtifactRevisionPayloads(ctx, tn.ID, keys)
	if err != nil {
		t.Fatalf("list payloads: %v", err)
	}

	want := map[ArtifactRevisionKey]ArtifactRevisionPayload{
		{ArtifactID: alpha.ID, RevisionNum: 1}: {
			Content: "ALPHA-BODY-1", MemorySeed: "ALPHA-SEED-1", MemoryScope: "project",
			Type: "subagent", Name: "alpha-one",
		},
		{ArtifactID: alpha.ID, RevisionNum: 2}: {
			Content: "ALPHA-BODY-2", MemorySeed: "ALPHA-SEED-2", MemoryScope: "user",
			Type: "subagent", Name: "alpha-two",
		},
		{ArtifactID: bravo.ID, RevisionNum: 1}: {
			Content: "BRAVO-BODY-1", Type: "skill", Name: "bravo-one",
		},
	}
	// Length first, and it is not a formality: a join that pairs on artifact_id
	// and drops the revision_num term returns alpha's third revision and
	// bravo's second as well, each correctly keyed, so every per-key assertion
	// below still passes while the read hands back revisions nobody asked for.
	if len(got) != len(want) {
		t.Fatalf("read returned %d payloads for %d keys: %+v", len(got), len(want), got)
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Fatalf("no payload for artifact %s revision %d. A result keyed on artifact_id alone "+
				"collapses the two revisions of one artifact onto a single entry, which is exactly "+
				"the pairing this key exists to hold apart. Got: %+v", k.ArtifactID, k.RevisionNum, got)
		}
		if g != w {
			t.Errorf("artifact %s revision %d:\n got %+v\nwant %+v", k.ArtifactID, k.RevisionNum, g, w)
		}
	}

	// Stated separately from the table above because it is the assertion the
	// rename exists for, and a table mismatch would not say so.
	if p := got[keys[2]]; p.Name != "alpha-one" {
		t.Errorf("revision 1 reports name %q, want alpha-one. The artifact is approved as %q now, so "+
			"reading identity off the live row instead of the revision puts revision 1's bytes on disk "+
			"at a path that version was never approved under", p.Name, alpha.ApprovedName)
	}
}

// TestListArtifactRevisionPayloadsFallsBackToApprovedIdentity covers the
// pre-00016 arm, which no fixture built entirely out of approvals can reach:
// every approval since 00016 records an identity, so a revision carrying NULL
// identity has to be written directly.
//
// insertRevision is the writer, with empty identity arguments, because its
// NULLIF turns those into the NULL a pre-00016 row holds. Going through a
// handler cannot produce this row: every handler supplies a real identity.
//
// Two things are pinned here and only one of them is the NULL branch. Passing
// the revision's NULL name through yields an EMPTY name, which is not a visible
// error anywhere downstream: it distributes a subagent at agents/.md. Falling
// back to the artifact's LIVE name instead of its APPROVED one is the other
// wrong answer, and it is the governance one, since the live name is whatever
// an admin last typed and no second admin has approved.
func TestListArtifactRevisionPayloadsFallsBackToApprovedIdentity(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	c, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "charlie-approved", Content: "C-BODY", Visibility: "org",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.insertRevision(ctx, tn.ID, c.ID, "C-LEGACY-BODY", "", "", "", "", "", nil, "", "approval", nil, "bob", 0); err != nil {
		t.Fatalf("insert identity-less revision: %v", err)
	}
	c = approveInTx(t, st, tn.ID, c.ID, "bob")

	// The working copy moves away from the approved identity WITHOUT a second
	// approval, which is what makes approved_name and name two different
	// strings and the COALESCE order falsifiable.
	c.Name = "charlie-draft"
	c, err = st.UpdateArtifact(ctx, c, c.RowVersion)
	if err != nil {
		t.Fatalf("rename working copy: %v", err)
	}
	if c.Name != "charlie-draft" || c.ApprovedName != "charlie-approved" {
		t.Fatalf("precondition: want live charlie-draft with approved charlie-approved, got %q/%q",
			c.Name, c.ApprovedName)
	}

	key := ArtifactRevisionKey{ArtifactID: c.ID, RevisionNum: 1}
	got, err := st.ListArtifactRevisionPayloads(ctx, tn.ID, []ArtifactRevisionKey{key})
	if err != nil {
		t.Fatalf("list payloads: %v", err)
	}
	p, ok := got[key]
	if !ok {
		t.Fatalf("no payload for the identity-less revision: %+v", got)
	}
	if p.Content != "C-LEGACY-BODY" {
		t.Errorf("content = %q, want C-LEGACY-BODY", p.Content)
	}
	if p.Type != "skill" || p.Name != "charlie-approved" {
		t.Errorf("identity-less revision resolved to %q/%q, want skill/charlie-approved. "+
			"An EMPTY name means the NULL arm is missing and this artifact distributes at skills//SKILL.md; "+
			"%q means the fallback read the LIVE row, distributing an identity no second admin approved",
			p.Type, p.Name, c.Name)
	}

	// The second COALESCE term, and it exists for the state RollbackArtifact's
	// own fallback documents: a withdrawn artifact has no approved identity at
	// all, because artifact_approved_identity_complete (00016) cleared it along
	// with the snapshot. The pinned sync path cannot reach this row (the
	// entitled query requires approved_content IS NOT NULL), so this is the
	// only place the branch is exercised at all; leaving it untested would mean
	// a mutant deleting it stays green.
	if _, err := st.WithdrawArtifact(ctx, tn.ID, c.ID); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	got, err = st.ListArtifactRevisionPayloads(ctx, tn.ID, []ArtifactRevisionKey{key})
	if err != nil {
		t.Fatalf("list payloads after withdraw: %v", err)
	}
	if p := got[key]; p.Type != "skill" || p.Name != "charlie-draft" {
		t.Errorf("after a withdraw the fallback resolved to %q/%q, want skill/charlie-draft: "+
			"with no approved identity left there is nothing but the live row to fall back to", p.Type, p.Name)
	}
}
