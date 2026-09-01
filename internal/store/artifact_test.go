package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestArtifactCRUD(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t) // same helper the other store tests use
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "reviewer",
		Description: "reviews code", Content: "---\nname: reviewer\ndescription: reviews code\n---\nbody",
		MemoryScope: "project", Version: "0.1.0",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.ID == "" || a.MemoryScope != "project" {
		t.Fatalf("bad create: %+v", a)
	}
	// New artifacts start as draft (status is retired; ApprovalState replaces it).
	if a.ApprovalState != "draft" {
		t.Fatalf("want draft on create, got %+v", a)
	}

	got, err := st.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil || got.Name != "reviewer" {
		t.Fatalf("get: %v %+v", err, got)
	}

	a.Description = "updated"
	up, err := st.UpdateArtifact(ctx, a, a.RowVersion)
	if err != nil || up.Description != "updated" || up.ApprovalState != "draft" {
		t.Fatalf("update: %v %+v", err, up)
	}

	// skill with no memory scope round-trips as empty
	sk, err := st.CreateArtifact(ctx, Artifact{TenantID: tn.ID, Type: "skill", Name: "fmt", Content: "x"})
	if err != nil || sk.MemoryScope != "" {
		t.Fatalf("skill: %v %+v", err, sk)
	}

	// Nothing is ever auto-approved: ListActiveOrgArtifacts (gated on
	// approved_content IS NOT NULL — the frozen snapshot, not the working
	// approval_state) sees neither artifact until the Task 3 approval workflow
	// exists to move one there.
	active, err := st.ListActiveOrgArtifacts(ctx, tn.ID)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("want 0 active (nothing approved yet), got %d", len(active))
	}

	if err := st.DeleteArtifact(ctx, tn.ID, sk.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetArtifact(ctx, tn.ID, sk.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestGetArtifactIsTenantScoped proves the STORE itself (not just the api
// layer's Go-level comparison) refuses to return an artifact for a tenant it
// does not belong to: GetArtifact must filter by tenant_id in SQL.
func TestGetArtifactIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tnA := mustTenant(t, st)
	tnB := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tnA.ID, Type: "skill", Name: "owned-by-a",
		Content: "---\nname: owned-by-a\ndescription: d\n---\nx",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Owning tenant can fetch it.
	if got, err := st.GetArtifact(ctx, tnA.ID, a.ID); err != nil || got.ID != a.ID {
		t.Fatalf("owning tenant get: %v %+v", err, got)
	}

	// A different tenant fetching the SAME id must get ErrNotFound from the store.
	if _, err := st.GetArtifact(ctx, tnB.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get: want ErrNotFound, got %v", err)
	}
}

// TestArtifactMalformedIDIsNotFound proves a non-UUID id is treated as
// ErrNotFound (mapping Postgres 22P02 invalid_text_representation), not
// surfaced as a raw driver error that would 500 at the API layer. Covers
// every artifact store function reachable directly from a handler with a raw
// path-value id (i.e. NOT preceded by a GetArtifactForUpdate call in the same
// tx, which would already have caught it).
func TestArtifactMalformedIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	const badID = "not-a-uuid"

	if _, err := st.GetArtifact(ctx, tn.ID, badID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetArtifact(bad id): want ErrNotFound, got %v", err)
	}

	// Mirror real handler usage: a failed GetArtifactForUpdate returns its
	// error straight out of the tx closure so InTx rolls back. (Postgres
	// aborts the whole transaction on the 22P02 cast failure — any later
	// query, including Commit, would fail on the poisoned tx; only Rollback
	// is valid, which is exactly what returning the error triggers.)
	err := st.InTx(ctx, func(tx *Store) error {
		_, e := tx.GetArtifactForUpdate(ctx, tn.ID, badID)
		return e
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetArtifactForUpdate(bad id): want ErrNotFound, got %v", err)
	}

	if err := st.DeleteArtifact(ctx, tn.ID, badID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteArtifact(bad id): want ErrNotFound, got %v", err)
	}

	// ListArtifactRevisions is Enterprise-only (artifact_revision.ee.go) and
	// is covered separately by TestArtifactRevisionMalformedIDIsNotFound
	// (artifact_revision.ee_test.go) — docs/specs/2026-08-19-orbeat-
	// community-repo-generation-design.md §4.
}

// TestArtifactTransitionMalformedIDIsNotFound is the direct, caller-
// independent proof for defect 3 of the 2026-09-01 fix (audit B37): transition
// (artifact.go), the shared helper behind SetArtifactSubmitted/Approved/
// Rejected/WithdrawArtifact/SetArtifactFindingsAcknowledged and
// RollbackArtifact (artifact_revision.ee.go), used to map only pgx.ErrNoRows
// to ErrNotFound — a malformed uuid id fell through to the generic %w wrap
// and 500'd instead of 404ing.
//
// Unreachable through every real handler (admin_artifact_review.go and
// admin_artifact_min_revision.ee.go all precede their transition-backed call
// with GetArtifactForUpdate inside the same tx, which already 404s a
// malformed id first — see TestArtifactMalformedIDIsNotFound above for that
// path), which is exactly why this test calls SetArtifactRejected directly,
// with no precedent GetArtifactForUpdate call: transition must defend
// against a malformed id itself, the same discipline every other store
// function in this package holds itself to.
func TestArtifactTransitionMalformedIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	const badID = "not-a-uuid"

	if _, err := st.SetArtifactRejected(ctx, tn.ID, badID, "no thanks"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetArtifactRejected(bad id): want ErrNotFound, got %v", err)
	}
}

func TestCreateRuleArtifact(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "rule", Name: "no-secrets",
		Description: "never commit secrets", Content: "Never commit credentials.",
		Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	got, err := st.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "rule" || got.Name != "no-secrets" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestRecordPublishFailurePreservesLastGood(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	t1 := time.Now().UTC().Truncate(time.Second)
	if err := st.RecordPublishSuccess(ctx, tn.ID, t1, "abc123"); err != nil {
		t.Fatalf("record success: %v", err)
	}

	// A failure must record itself WITHOUT disturbing the last good publish.
	t2 := t1.Add(time.Minute)
	if err := st.RecordPublishFailure(ctx, tn.ID, t2, "boom"); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	ps, err := st.GetPublishState(ctx, tn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ps.LastCommit != "abc123" {
		t.Fatalf("failure wiped last_commit: %+v", ps)
	}
	if ps.LastSuccessAt == nil || !ps.LastSuccessAt.Equal(t1) {
		t.Fatalf("failure wiped last_success_at: %+v", ps)
	}
	if ps.LastError != "boom" {
		t.Fatalf("last_error not recorded: %+v", ps)
	}
	if ps.LastAttemptAt == nil || !ps.LastAttemptAt.Equal(t2) {
		t.Fatalf("last_attempt_at not advanced: %+v", ps)
	}

	// A later success clears the error and advances the commit.
	t3 := t2.Add(time.Minute)
	if err := st.RecordPublishSuccess(ctx, tn.ID, t3, "def456"); err != nil {
		t.Fatalf("record success 2: %v", err)
	}
	ps, err = st.GetPublishState(ctx, tn.ID)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if ps.LastError != "" {
		t.Fatalf("success did not clear last_error: %+v", ps)
	}
	if ps.LastCommit != "def456" {
		t.Fatalf("success did not advance last_commit: %+v", ps)
	}
	if ps.LastSuccessAt == nil || !ps.LastSuccessAt.Equal(t3) {
		t.Fatalf("success did not advance last_success_at: %+v", ps)
	}
	if ps.LastAttemptAt == nil || !ps.LastAttemptAt.Equal(t3) {
		t.Fatalf("success did not advance last_attempt_at: %+v", ps)
	}
}

// TestRecordPublishFailureOnFreshTenant pins the INSERT path: a first-ever
// failure must land even though the statement never names last_commit /
// last_success_at (the column defaults cover it).
func TestRecordPublishFailureOnFreshTenant(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	now := time.Now().UTC().Truncate(time.Second)
	if err := st.RecordPublishFailure(ctx, tn.ID, now, "boom"); err != nil {
		t.Fatalf("record failure on fresh tenant: %v", err)
	}
	ps, err := st.GetPublishState(ctx, tn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ps.LastCommit != "" || ps.LastSuccessAt != nil {
		t.Fatalf("fresh failure invented a publish: %+v", ps)
	}
	if ps.LastError != "boom" {
		t.Fatalf("last_error not recorded: %+v", ps)
	}
}

func TestArtifactVisibility(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	// Default visibility is 'org' when unset (backward-compatible with 2-b-1 rows).
	org, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "org-skill",
		Content: "---\nname: org-skill\ndescription: d\n---\nx",
	})
	if err != nil || org.Visibility != "org" {
		t.Fatalf("default visibility: err=%v art=%+v", err, org)
	}

	// Explicit role visibility round-trips.
	role, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "role-skill",
		Content: "---\nname: role-skill\ndescription: d\n---\nx", Visibility: "role",
	})
	if err != nil || role.Visibility != "role" {
		t.Fatalf("role visibility: err=%v art=%+v", err, role)
	}

	// Neither artifact has an approval yet, so ListActiveOrgArtifacts (gated on
	// approved_content IS NOT NULL) returns nothing for either. The full
	// SECURITY INVARIANT — role artifacts can never leak into the org/plugin
	// read path even once approved — is reinstated with an approval-driven
	// fixture by TestDistributionServesApprovedSnapshotNotWorkingCopy (P4 Task 2).
	orgs, err := st.ListActiveOrgArtifacts(ctx, tn.ID)
	if err != nil {
		t.Fatalf("list org: %v", err)
	}
	if len(orgs) != 0 {
		t.Fatalf("ListActiveOrgArtifacts should return nothing pre-approval, got %+v", orgs)
	}
}

func TestArtifactMemoryScopeRejectedForSkill(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	_, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "bad-skill",
		Content: "x", MemoryScope: "user",
	})
	if err == nil {
		t.Fatal("expected DB CHECK to reject memory_scope on a skill, got nil error")
	}
}

func TestArtifactMemorySeedRejectedForSkill(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	_, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "bad-seed",
		Content: "x", MemorySeed: "seed on a skill",
	})
	if err == nil {
		t.Fatalf("want DB CHECK rejection for memory_seed on a skill")
	}
}

func TestArtifactMemorySeedRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "seeded",
		Content:     "---\nname: seeded\ndescription: d\n---\nbody",
		MemoryScope: "user", MemorySeed: "## Standards\nUse table-driven tests.",
		Version: "0.1.0", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.MemorySeed != "## Standards\nUse table-driven tests." {
		t.Fatalf("seed not returned on create: %+v", a)
	}

	got, err := st.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil || got.MemorySeed != a.MemorySeed {
		t.Fatalf("get: %v seed=%q", err, got.MemorySeed)
	}

	// Empty seed round-trips as "" (stored as NULL).
	a.MemorySeed = ""
	up, err := st.UpdateArtifact(ctx, a, a.RowVersion)
	if err != nil || up.MemorySeed != "" {
		t.Fatalf("update to empty: %v %+v", err, up)
	}

	// The entitled-list read path (distribution projection) must carry the seed
	// from the APPROVED snapshot — approve after setting "seed v2" so it's what
	// gets frozen. up.RowVersion (not a.RowVersion, now stale by one update) is
	// the row's current version.
	a.MemorySeed = "seed v2"
	if _, err := st.UpdateArtifact(ctx, a, up.RowVersion); err != nil {
		t.Fatalf("update: %v", err)
	}
	approveArtifact(t, st, tn.ID, a.ID)
	role, _ := st.CreateRole(ctx, tn.ID, "sec")
	_, _ = st.CreateArtifactEntitlement(ctx, ArtifactEntitlement{TenantID: tn.ID, RoleID: role.ID, ArtifactID: a.ID})
	ents, err := st.ListEntitledArtifacts(ctx, tn.ID, []string{role.ID})
	if err != nil || len(ents) != 1 || ents[0].MemorySeed != "seed v2" {
		t.Fatalf("entitled list must carry MemorySeed: %v %+v", err, ents)
	}
	if ents[0].MemoryScope != "user" {
		t.Fatalf("entitled list must carry approved MemoryScope: %+v", ents)
	}
}

func TestArtifactApprovalRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "rev",
		Content:     "---\nname: rev\ndescription: d\n---\nbody",
		MemoryScope: "user", MemorySeed: "seed v1", Version: "0.1.0", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// New artifacts start as draft with no approved snapshot.
	if a.ApprovalState != "draft" || a.ApprovedContent != "" {
		t.Fatalf("new artifact must be draft w/o snapshot: %+v", a)
	}
	if string(a.ScanFindings) != "[]" {
		t.Fatalf("scan_findings default must be []: %q", a.ScanFindings)
	}

	got, err := st.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil || got.ApprovalState != "draft" {
		t.Fatalf("get: %v %+v", err, got)
	}

	// Editing re-dirties to draft and never populates the snapshot.
	a.Content = "---\nname: rev\ndescription: d2\n---\nedited"
	up, err := st.UpdateArtifact(ctx, a, a.RowVersion)
	if err != nil || up.ApprovalState != "draft" || up.ApprovedContent != "" {
		t.Fatalf("update: %v %+v", err, up)
	}
}

func TestArtifactTransitions(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	a, _ := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "rev",
		Content: "---\nname: rev\ndescription: d\n---\nWORK", MemoryScope: "user",
		MemorySeed: "seed", Visibility: "role",
	})

	// submit, with a digest alongside the findings (migration 00028). The
	// store does not compute this value itself (govern.Digest is the API
	// layer's job, keeping this package govern-free), it only has to
	// persist whatever the caller hands it, in the SAME statement that
	// writes scan_findings, so this is a plain round-trip check.
	sub, err := st.SetArtifactSubmitted(ctx, tn.ID, a.ID, "alice", []byte(`[{"rule":"x","severity":"warn"}]`), "fake-digest-1")
	if err != nil || sub.ApprovalState != "pending" || sub.SubmittedBy != "alice" {
		t.Fatalf("submit: %v %+v", err, sub)
	}
	if string(sub.ScanFindings) != `[{"rule":"x","severity":"warn"}]` {
		t.Fatalf("findings not persisted: %q", sub.ScanFindings)
	}
	if sub.ScanFindingsDigest != "fake-digest-1" {
		t.Fatalf("digest not persisted: got %q, want %q", sub.ScanFindingsDigest, "fake-digest-1")
	}

	// approve → snapshot captured from working columns + revision appended.
	// SetArtifactApproved now appends a revision, so it must run in a tx.
	var app Artifact
	if err := st.InTx(ctx, func(tx *Store) error {
		if _, e := tx.GetArtifactForUpdate(ctx, tn.ID, a.ID); e != nil {
			return e
		}
		var e error
		app, _, e = tx.SetArtifactApproved(ctx, tn.ID, a.ID, "bob", 0)
		return e
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if app.ApprovalState != "approved" || app.ApprovedBy != "bob" {
		t.Fatalf("approve state: %+v", app)
	}
	if app.ApprovedContent != "---\nname: rev\ndescription: d\n---\nWORK" || app.ApprovedMemorySeed != "seed" || app.ApprovedMemoryScope != "user" {
		t.Fatalf("snapshot not captured: %+v", app)
	}

	// reject → recorded, but the already-approved snapshot MUST stay live
	// (rejecting a fresh submission never un-publishes what is already approved).
	rej, err := st.SetArtifactRejected(ctx, tn.ID, a.ID, "too risky")
	if err != nil || rej.ApprovalState != "rejected" || rej.RejectReason != "too risky" {
		t.Fatalf("reject: %v %+v", err, rej)
	}
	if rej.ApprovedContent != "---\nname: rev\ndescription: d\n---\nWORK" {
		t.Fatalf("reject must not clear the live approved snapshot: %+v", rej)
	}

	// resubmit supersedes the rejection: pending again, reject_reason cleared.
	// An empty digest string must round-trip as "" (stored as NULL via
	// NULLIF), matching Artifact.ScanFindingsDigest's ""-means-NULL contract
	//, not the literal string of the PREVIOUS submit's digest left behind.
	resub, err := st.SetArtifactSubmitted(ctx, tn.ID, a.ID, "alice", []byte(`[]`), "")
	if err != nil || resub.ApprovalState != "pending" || resub.RejectReason != "" {
		t.Fatalf("resubmit must clear reject_reason: %v %+v", err, resub)
	}
	if resub.ScanFindingsDigest != "" {
		t.Fatalf("resubmit with an empty digest kept the previous one: %q", resub.ScanFindingsDigest)
	}

	// withdraw → snapshot fully cleared (content + approver + timestamp), back to draft.
	wd, err := st.WithdrawArtifact(ctx, tn.ID, a.ID)
	if err != nil || wd.ApprovalState != "draft" || wd.ApprovedContent != "" {
		t.Fatalf("withdraw: %v %+v", err, wd)
	}
	if wd.ApprovedBy != "" || wd.ApprovedAt != nil {
		t.Fatalf("withdraw must clear approver + approved_at: %+v", wd)
	}

	// unknown id → ErrNotFound (still surfaced from within a tx)
	errUnknown := st.InTx(ctx, func(tx *Store) error {
		_, _, e := tx.SetArtifactApproved(ctx, tn.ID, "00000000-0000-0000-0000-000000000000", "bob", 0)
		return e
	})
	if !errors.Is(errUnknown, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", errUnknown)
	}
}

func TestDistributionServesApprovedSnapshotNotWorkingCopy(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	// Draft org artifact: never approved → invisible to distribution.
	_, _ = st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "draftskill",
		Content: "---\nname: draftskill\ndescription: d\n---\nx", Visibility: "org",
	})
	org := st.mustListActiveOrg(t, tn.ID)
	if len(org) != 0 {
		t.Fatalf("unapproved artifact must not distribute: %+v", org)
	}

	// Approve one, then edit the working copy — distribution must still serve the snapshot.
	a, _ := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "liveskill",
		Content: "---\nname: liveskill\ndescription: d\n---\nAPPROVED", Visibility: "org",
	})
	var approved Artifact
	if err := st.InTx(ctx, func(tx *Store) error {
		if _, e := tx.GetArtifactForUpdate(ctx, tn.ID, a.ID); e != nil {
			return e
		}
		var e error
		approved, _, e = tx.SetArtifactApproved(ctx, tn.ID, a.ID, "approver", 0)
		return e
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// approved.RowVersion (not a.RowVersion, now stale — SetArtifactApproved
	// bumped it without touching the local a variable) is the row's current version.
	a.Content = "---\nname: liveskill\ndescription: d\n---\nEDITED-WORKING"
	if _, err := st.UpdateArtifact(ctx, a, approved.RowVersion); err != nil {
		t.Fatalf("edit: %v", err)
	}
	org = st.mustListActiveOrg(t, tn.ID)
	if len(org) != 1 || org[0].Content != "---\nname: liveskill\ndescription: d\n---\nAPPROVED" {
		t.Fatalf("must serve approved snapshot, not working edit: %+v", org)
	}
}

// mustListActiveOrg is a tiny helper kept in the test file.
func (s *Store) mustListActiveOrg(t *testing.T, tenantID string) []Artifact {
	t.Helper()
	got, err := s.ListActiveOrgArtifacts(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("list org: %v", err)
	}
	return got
}

// approvedIdentityColumns reads an artifact's three approved identity columns
// straight out of the table as nullable strings, so a test can tell "" (an
// empty value the Go struct also reports for NULL) apart from a real NULL.
func approvedIdentityColumns(t *testing.T, st *Store, id string) (atype, name, vis *string) {
	t.Helper()
	err := st.db.QueryRow(context.Background(),
		`SELECT approved_type, approved_name, approved_visibility FROM artifact WHERE id=$1`, id).
		Scan(&atype, &name, &vis)
	if err != nil {
		t.Fatalf("read approved identity columns: %v", err)
	}
	return atype, name, vis
}

// TestApprovalSnapshotsIdentityAndEditLeavesItAlone is the store half of the
// slice's whole point: approval freezes type/name/visibility next to the
// content, a later edit to any of them moves only the live row, and withdraw
// clears both halves together.
//
// The assertion that matters is the third one. Asserting only that approval
// copies the identity across proves nothing about this design, because before
// 00016 the live columns WERE the distributed identity and any read of them
// agreed with the snapshot trivially. The rename in the middle is what forces
// the two apart, and it is the state every later task depends on existing.
func TestApprovalSnapshotsIdentityAndEditLeavesItAlone(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "ident-old",
		Content: "---\nname: ident-old\ndescription: d\n---\nV1", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var app Artifact
	if err := st.InTx(ctx, func(tx *Store) error {
		if _, e := tx.GetArtifactForUpdate(ctx, tn.ID, a.ID); e != nil {
			return e
		}
		var e error
		app, _, e = tx.SetArtifactApproved(ctx, tn.ID, a.ID, "bob", 0)
		return e
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if app.ApprovedType != "subagent" || app.ApprovedName != "ident-old" || app.ApprovedVisibility != "role" {
		t.Fatalf("approval did not snapshot the identity: type=%q name=%q visibility=%q",
			app.ApprovedType, app.ApprovedName, app.ApprovedVisibility)
	}

	// The appended revision is the COMPLETE approved state, identity included:
	// a revision holding content but not its name restores old content under
	// the current name on rollback, which is the desync this slice prevents.
	var revType, revName, revVis *string
	if err := st.db.QueryRow(ctx,
		`SELECT type, name, visibility FROM artifact_revision WHERE artifact_id=$1 AND revision_num=1`, a.ID).
		Scan(&revType, &revName, &revVis); err != nil {
		t.Fatalf("read revision 1: %v", err)
	}
	if revType == nil || revName == nil || revVis == nil {
		t.Fatalf("revision 1 recorded no identity: type=%v name=%v visibility=%v", revType, revName, revVis)
	}
	if *revType != "subagent" || *revName != "ident-old" || *revVis != "role" {
		t.Fatalf("revision 1 identity = %q/%q/%q, want subagent/ident-old/role", *revType, *revName, *revVis)
	}

	// Rename and flip the channel. Only the live row moves.
	app.Name = "ident-new"
	app.Visibility = "org"
	edited, err := st.UpdateArtifact(ctx, app, app.RowVersion)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if edited.Name != "ident-new" || edited.Visibility != "org" {
		t.Fatalf("the live row did not move: name=%q visibility=%q", edited.Name, edited.Visibility)
	}
	if edited.ApprovedName != "ident-old" || edited.ApprovedVisibility != "role" || edited.ApprovedType != "subagent" {
		t.Fatalf("the edit moved the distributed identity: approved type=%q name=%q visibility=%q, want subagent/ident-old/role",
			edited.ApprovedType, edited.ApprovedName, edited.ApprovedVisibility)
	}
	if edited.ApprovalState != "draft" {
		t.Fatalf("an identity edit must re-dirty the working copy: approval_state=%q", edited.ApprovalState)
	}

	// Withdraw clears identity with the rest of the snapshot. Read the raw
	// columns, not the struct: the struct reports "" for NULL and for an empty
	// string alike, so it cannot tell a cleared column from a blanked one.
	wd, err := st.WithdrawArtifact(ctx, tn.ID, a.ID)
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if wd.ApprovedType != "" || wd.ApprovedName != "" || wd.ApprovedVisibility != "" {
		t.Fatalf("withdraw left an identity behind: %q/%q/%q", wd.ApprovedType, wd.ApprovedName, wd.ApprovedVisibility)
	}
	gotType, gotName, gotVis := approvedIdentityColumns(t, st, a.ID)
	if gotType != nil || gotName != nil || gotVis != nil {
		t.Fatalf("withdraw must NULL the identity columns, got type=%v name=%v visibility=%v", gotType, gotName, gotVis)
	}
}

// TestApprovedIdentityCheckRejectsHalfSnapshots pins
// artifact_approved_identity_complete (00016 step 4) in both directions, then
// proves it is the CHECK and not the partial unique index that closes the
// hole.
//
// That last part is the reason this test is longer than a two-line constraint
// check. artifact_tenant_approved_identity_uniq is partial on
// approved_content IS NOT NULL and a btree treats NULLs as DISTINCT, so two
// rows carrying a snapshot with a NULL approved_name both sit inside that
// index conflicting with nothing, and both would distribute under an empty
// name. The middle section drops the constraint inside a transaction it then
// rolls back and watches both writes land, which is the shape the index
// cannot catch: without it, "the index protects us" reads as true and is not.
func TestApprovedIdentityCheckRejectsHalfSnapshots(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	mk := func(name string) Artifact {
		t.Helper()
		a, err := st.CreateArtifact(ctx, Artifact{
			TenantID: tn.ID, Type: "skill", Name: name,
			Content: "---\nname: " + name + "\ndescription: d\n---\nbody", Visibility: "org",
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return a
	}
	a, b := mk("half-a"), mk("half-b")

	// A snapshot with no identity.
	_, err := st.db.Exec(ctx, `UPDATE artifact SET approved_content='SNAPSHOT' WHERE id=$1`, a.ID)
	if !isConstraintViolation(err, "artifact_approved_identity_complete") {
		t.Fatalf("setting approved_content without an identity must violate "+
			"artifact_approved_identity_complete, got %v", err)
	}

	// The mirror: an identity with no snapshot. Inert today, one WHERE clause
	// away from mattering, and it is the state WithdrawArtifact would leave if
	// it nulled the content and forgot the name.
	_, err = st.db.Exec(ctx, `UPDATE artifact SET approved_name='half-a' WHERE id=$1`, a.ID)
	if !isConstraintViolation(err, "artifact_approved_identity_complete") {
		t.Fatalf("setting approved_name without a snapshot must violate "+
			"artifact_approved_identity_complete, got %v", err)
	}

	// What the index alone cannot see. Everything below happens inside a
	// transaction that is rolled back, so the constraint survives the test.
	var landed int
	sentinel := errors.New("roll back the dropped constraint")
	var inner error
	txErr := st.InTx(ctx, func(tx *Store) error {
		if _, e := tx.db.Exec(ctx, `ALTER TABLE artifact DROP CONSTRAINT artifact_approved_identity_complete`); e != nil {
			inner = e
			return sentinel
		}
		for _, id := range []string{a.ID, b.ID} {
			if _, e := tx.db.Exec(ctx, `UPDATE artifact SET approved_content='SNAPSHOT' WHERE id=$1`, id); e != nil {
				inner = e
				return sentinel
			}
		}
		if e := tx.db.QueryRow(ctx, `SELECT count(*) FROM artifact
			WHERE tenant_id=$1 AND approved_content IS NOT NULL AND approved_name IS NULL`, tn.ID).Scan(&landed); e != nil {
			inner = e
			return sentinel
		}
		return sentinel
	})
	if !errors.Is(txErr, sentinel) {
		t.Fatalf("the constraint-drop transaction must roll back, got %v", txErr)
	}
	if inner != nil {
		t.Fatalf("with the CHECK dropped, both identity-less snapshots must land; got %v", inner)
	}
	if landed != 2 {
		t.Fatalf("with the CHECK dropped, %d identity-less snapshots landed, want 2. "+
			"If this is 1 the unique index caught the second one and the CHECK is not load-bearing; "+
			"if it is 0 this red-proof is measuring nothing", landed)
	}

	// The rollback really put the constraint back.
	_, err = st.db.Exec(ctx, `UPDATE artifact SET approved_content='SNAPSHOT' WHERE id=$1`, b.ID)
	if !isConstraintViolation(err, "artifact_approved_identity_complete") {
		t.Fatalf("the CHECK did not survive the rolled-back drop, got %v", err)
	}
}

// TestApprovedIdentityUniqueIndexGuardsTheDistributedNamespace walks the
// collision 00016 step 5 exists for. 00003's UNIQUE (tenant_id, type, name)
// is on the LIVE columns and cannot see it: once A is renamed, A's live name
// is free, so B can take it, and only B's APPROVAL puts two rows into
// distribution under one path.
func TestApprovedIdentityUniqueIndexGuardsTheDistributedNamespace(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	approve := func(id string) error {
		return st.InTx(ctx, func(tx *Store) error {
			if _, e := tx.GetArtifactForUpdate(ctx, tn.ID, id); e != nil {
				return e
			}
			_, _, e := tx.SetArtifactApproved(ctx, tn.ID, id, "bob", 0)
			return e
		})
	}

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "dup-foo",
		Content: "---\nname: dup-foo\ndescription: d\n---\nA", Visibility: "org",
	})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := approve(a.ID); err != nil {
		t.Fatalf("approve a: %v", err)
	}

	// A moves off the name it is still distributing under.
	cur, err := st.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	cur.Name = "dup-bar"
	if _, err := st.UpdateArtifact(ctx, cur, cur.RowVersion); err != nil {
		t.Fatalf("rename a: %v", err)
	}

	// The live constraint allows B to take the freed live name. That it does
	// is a precondition of this test, not the thing being tested: if this
	// create started failing, the collision below would be unreachable and the
	// index assertion would pass for the wrong reason.
	b, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "dup-foo",
		Content: "---\nname: dup-foo\ndescription: d\n---\nB", Visibility: "org",
	})
	if err != nil {
		t.Fatalf("create b under a's freed live name: %v", err)
	}

	err = approve(b.ID)
	if !isConstraintViolation(err, "artifact_tenant_approved_identity_uniq") {
		t.Fatalf("approving b must collide with a's distributed identity, got %v", err)
	}

	var distributing int
	if err := st.db.QueryRow(ctx, `SELECT count(*) FROM artifact
		WHERE tenant_id=$1 AND approved_type='skill' AND approved_name='dup-foo'`, tn.ID).Scan(&distributing); err != nil {
		t.Fatalf("count: %v", err)
	}
	if distributing != 1 {
		t.Fatalf("%d artifacts distribute as skill/dup-foo, want exactly 1", distributing)
	}

	// The index is PARTIAL on approved_content IS NOT NULL: pulling A out of
	// distribution frees the name for B, which is the "A withdrawn instead of
	// approved" interleaving. A predicate-less unique index would keep
	// refusing here.
	if _, err := st.WithdrawArtifact(ctx, tn.ID, a.ID); err != nil {
		t.Fatalf("withdraw a: %v", err)
	}
	if err := approve(b.ID); err != nil {
		t.Fatalf("after a is withdrawn, b must approve cleanly: %v", err)
	}
}

// TestArtifactSlimProjectionCarriesRealMinRevisionNum pins the VALUE of
// min_revision_num in the slim list projection, which is a different claim from
// the one the existing parity coverage makes.
// TestArtifactPageSlimOmitsHeavyFieldsButKeepsApprovedFlag pins only that
// artifactSlimCols has the same column COUNT as artifactCols, so replacing the
// floor with a same-shaped `0::int AS min_revision_num` would satisfy the count
// and slip through with every test green.
//
// The floor has to survive the slimming because it is an int and not one of the
// four heavy payload columns that projection exists to drop, and because the
// admin list row shows it without ?include=content. A constant there would show
// every artifact as unfloored on the one screen an admin uses to see which ones
// are.
//
// The by-id read is asserted in the same test on purpose. It is the control:
// blank the floor out of artifactSlimCols alone and this test goes red on the
// list while staying green on GetArtifact, which is what identifies WHICH of
// the two projections broke. A test that only read one of them would report the
// same failure for either.
func TestArtifactSlimProjectionCarriesRealMinRevisionNum(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "slim-floor",
		Content: "---\nname: slim-floor\ndescription: d\n---\nbody\n",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.MinRevisionNum != 0 {
		t.Fatalf("a freshly created artifact has min_revision_num = %d, want the 0 no-floor "+
			"default from migration 00018", a.MinRevisionNum)
	}

	// Written directly: PUT .../min-revision is a later task. The value is
	// non-zero so the assertions below cannot be satisfied by a projection
	// that stopped reading the column, and it is not 1 so it cannot be
	// satisfied by one that confuses the floor with a revision number.
	const floor = 6
	if _, err := st.db.Exec(ctx,
		`UPDATE artifact SET min_revision_num=$1 WHERE id=$2`, floor, a.ID); err != nil {
		t.Fatalf("set min_revision_num: %v", err)
	}

	full, err := st.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil {
		t.Fatalf("get full: %v", err)
	}
	if full.MinRevisionNum != floor {
		t.Fatalf("GetArtifact min_revision_num = %d, want %d: artifactCols must project the "+
			"artifact's real floor", full.MinRevisionNum, floor)
	}

	slim, err := st.ListArtifactsPage(ctx, tn.ID, ArtifactPageOpts{Limit: 100})
	if err != nil || len(slim) != 1 {
		t.Fatalf("slim list = %d rows, err=%v; want 1", len(slim), err)
	}
	if slim[0].MinRevisionNum != floor {
		t.Fatalf("slim list min_revision_num = %d, want %d: artifactSlimCols must carry the "+
			"artifact's REAL floor, not a same-count placeholder, or the admin list shows every "+
			"artifact as unfloored", slim[0].MinRevisionNum, floor)
	}
}

// TestArtifactMinRevisionFloorRejectsNegative pins migration 00018's CHECK.
//
// The write goes through direct SQL rather than a handler deliberately: the
// CONSTRAINT is the assertion, and a handler-driven attempt would be stopped by
// request validation long before Postgres saw it, proving something about the
// handler and nothing about the schema. Remove the CHECK from 00018 and this
// test is the only thing in the tree that notices.
//
// It asserts by CONSTRAINT NAME rather than by SQLSTATE, because artifact
// carries several CHECKs (type, memory_scope, approval_state, the approved
// identity) and "some integrity constraint fired" would pass on the wrong one
// while reading as proof of the right one.
//
// Zero is asserted to be ACCEPTED in the same test, and that is not padding: it
// is what separates `>= 0` from `>= 1`. 0 is the no-floor sentinel every
// existing row carries by default, so a CHECK written one off would reject the
// modal value of the column while still refusing -1.
func TestArtifactMinRevisionFloorRejectsNegative(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "floor-check",
		Content: "---\nname: floor-check\ndescription: d\n---\nbody\n",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err = st.db.Exec(ctx, `UPDATE artifact SET min_revision_num=-1 WHERE id=$1`, a.ID)
	if !isConstraintViolation(err, "artifact_min_revision_num_non_negative") {
		t.Fatalf("min_revision_num = -1 must violate artifact_min_revision_num_non_negative, got %v. "+
			"insertRevision numbers from 1 and 0 means no floor, so a negative floor is a value no "+
			"clamp has a rule for", err)
	}

	if _, err := st.db.Exec(ctx, `UPDATE artifact SET min_revision_num=0 WHERE id=$1`, a.ID); err != nil {
		t.Fatalf("min_revision_num = 0 must be ACCEPTED (it is the no-floor sentinel every row "+
			"defaults to), got %v", err)
	}
}

// TestArtifactFindingsAcknowledgmentFreshReadsUnacknowledged proves a freshly
// created artifact reads as "no digest, not acknowledged" with no backfill
// step involved: migration 00028's four columns are all nullable, and a
// draft that has never been submitted has no findings to have a digest over.
func TestArtifactFindingsAcknowledgmentFreshReadsUnacknowledged(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "ack-fresh",
		Content: "---\nname: ack-fresh\ndescription: d\n---\nbody",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.ScanFindingsDigest != "" || a.FindingsAckDigest != "" || a.FindingsAckBy != "" || a.FindingsAckAt != nil {
		t.Fatalf("fresh artifact must read as not acknowledged, got %+v", a)
	}

	// GetArtifact must agree independently of CreateArtifact's own RETURNING.
	got, err := st.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ScanFindingsDigest != "" || got.FindingsAckDigest != "" || got.FindingsAckBy != "" || got.FindingsAckAt != nil {
		t.Fatalf("GetArtifact of a fresh artifact must read as not acknowledged, got %+v", got)
	}
}

// TestSetArtifactFindingsAcknowledgedRecordsDigestActorAndTime proves
// SetArtifactFindingsAcknowledged records all three facts of an
// acknowledgment (digest, actor, time), and that the write survives an
// independent re-read through GetArtifact, not merely the UPDATE's own
// RETURNING clause.
func TestSetArtifactFindingsAcknowledgedRecordsDigestActorAndTime(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "ack-record",
		Content: "---\nname: ack-record\ndescription: d\n---\nbody",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	before := time.Now().Add(-time.Second)
	acked, err := st.SetArtifactFindingsAcknowledged(ctx, tn.ID, a.ID, "alice", "digest-1")
	if err != nil {
		t.Fatalf("set acknowledged: %v", err)
	}
	if acked.FindingsAckDigest != "digest-1" || acked.FindingsAckBy != "alice" {
		t.Fatalf("acknowledgment not recorded: %+v", acked)
	}
	if acked.FindingsAckAt == nil || acked.FindingsAckAt.Before(before) {
		t.Fatalf("FindingsAckAt not set to now(): %+v", acked.FindingsAckAt)
	}

	got, err := st.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FindingsAckDigest != "digest-1" || got.FindingsAckBy != "alice" || got.FindingsAckAt == nil {
		t.Fatalf("acknowledgment did not persist through an independent read: %+v", got)
	}
}

// TestSetArtifactSubmittedClearsPriorFindingsAcknowledgment replaces
// TestClearArtifactFindingsAcknowledgmentResetsToUnacknowledged, whose
// subject (ClearArtifactFindingsAcknowledgment) was deleted on 2026-08-28
// having never had a non-test caller. That test proved the reset in
// isolation; this one proves it where it actually has to happen, in
// SetArtifactSubmitted's own statement, which is the only thing that makes a
// resubmit invalidate the previous cycle's acknowledgment.
//
// The second submitter differs from the first and the digest is IDENTICAL
// across both rounds, which is the reject-then-resubmit-unchanged case: with
// the same digest, every downstream digest comparison still matches, so the
// clearing of findings_ack_by is the only thing distinguishing a genuine
// acknowledgment from an inherited one.
func TestSetArtifactSubmittedClearsPriorFindingsAcknowledgment(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "ack-resubmit",
		Content: "---\nname: ack-resubmit\ndescription: d\n---\nbody",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.SetArtifactSubmitted(ctx, tn.ID, a.ID, "alice", []byte(`[{"rule":"r"}]`), "digest-1"); err != nil {
		t.Fatalf("submit round 1: %v", err)
	}
	if _, err := st.SetArtifactFindingsAcknowledged(ctx, tn.ID, a.ID, "alice", "digest-1"); err != nil {
		t.Fatalf("set acknowledged: %v", err)
	}

	resubmitted, err := st.SetArtifactSubmitted(ctx, tn.ID, a.ID, "bob", []byte(`[{"rule":"r"}]`), "digest-1")
	if err != nil {
		t.Fatalf("submit round 2: %v", err)
	}
	if resubmitted.SubmittedBy != "bob" || resubmitted.ScanFindingsDigest != "digest-1" {
		t.Fatalf("setup: round 2 must move the submitter and keep the digest, got %+v", resubmitted)
	}
	if resubmitted.FindingsAckDigest != "" || resubmitted.FindingsAckBy != "" || resubmitted.FindingsAckAt != nil {
		t.Fatalf("a resubmission must not inherit the prior acknowledgment, got ackBy=%q ackDigest=%q ackAt=%v",
			resubmitted.FindingsAckBy, resubmitted.FindingsAckDigest, resubmitted.FindingsAckAt)
	}

	got, err := st.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FindingsAckDigest != "" || got.FindingsAckBy != "" || got.FindingsAckAt != nil {
		t.Fatalf("the clear did not persist through an independent read: %+v", got)
	}
}

// TestArtifactFindingsAcknowledgmentIsTenantScoped is the cross-tenant proof
// the CTE trap demands: a WHERE clause missing tenant_id on only the UPDATE
// half of a two-CTE statement can mutate a foreign row while the
// existence-check half still, correctly but now incompletely, reports
// ErrNotFound (exactly what the role-rename slice found in UpdateRoleName).
// Both writers are single-statement UPDATEs here, not two-CTE ones, but the
// proof is the same shape regardless of the SQL used to produce it: read the
// FOREIGN row back through its OWNING tenant, never through the attacking
// tenant, so a bug that mutates the row while still reporting ErrNotFound
// cannot pass by accident of reading it back the same wrong way it was
// written.
//
// The second arm was ClearArtifactFindingsAcknowledgment until 2026-08-28,
// when that function was deleted for having no non-test caller. It is now
// SetArtifactSubmitted, which is the function that clears these three columns
// in production, so the arm still covers a real writer of them rather than
// being dropped along with the function it named.
func TestArtifactFindingsAcknowledgmentIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tnA := mustTenant(t, st)
	tnB := mustTenant(t, st)

	foreign, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tnB.ID, Type: "skill", Name: "ack-foreign",
		Content: "---\nname: ack-foreign\ndescription: d\n---\nbody",
	})
	if err != nil {
		t.Fatalf("create foreign: %v", err)
	}
	// The foreign row starts with a REAL acknowledgment, written through its
	// owning tenant: a cross-tenant clear that wrongly succeeded would be
	// invisible against a row whose columns were already empty.
	if _, err := st.SetArtifactFindingsAcknowledged(ctx, tnB.ID, foreign.ID, "owner", "digest-owned"); err != nil {
		t.Fatalf("seed the foreign acknowledgment: %v", err)
	}

	if _, err := st.SetArtifactFindingsAcknowledged(ctx, tnA.ID, foreign.ID, "mallory", "digest-evil"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant SetArtifactFindingsAcknowledged: want ErrNotFound, got %v", err)
	}
	if _, err := st.SetArtifactSubmitted(ctx, tnA.ID, foreign.ID, "mallory", []byte("[]"), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant SetArtifactSubmitted: want ErrNotFound, got %v", err)
	}

	still, err := st.GetArtifact(ctx, tnB.ID, foreign.ID)
	if err != nil {
		t.Fatalf("get foreign after cross-tenant attempts: %v", err)
	}
	if still.FindingsAckDigest != "digest-owned" || still.FindingsAckBy != "owner" || still.FindingsAckAt == nil {
		t.Fatalf("cross-tenant calls mutated the foreign artifact's acknowledgment despite reporting ErrNotFound: %+v", still)
	}
	if still.SubmittedBy != "" || still.ApprovalState != "draft" {
		t.Fatalf("cross-tenant SetArtifactSubmitted moved the foreign artifact's review state despite reporting ErrNotFound: %+v", still)
	}
}

// TestArtifactFindingsAckCompleteConstraint pins migration 00028's
// artifact_findings_ack_complete CHECK: the three acknowledgment columns must
// be written together or not at all, the same defect class 00016's
// artifact_approved_identity_complete closes for the approved-identity
// columns. scan_findings_digest is deliberately NOT part of this CHECK and is
// not exercised here: a digest can exist unacknowledged, and an
// acknowledgment can exist for a digest that no longer matches (a stale
// acknowledgment after a re-scan) -- both are valid states, not violations.
func TestArtifactFindingsAckCompleteConstraint(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "ack-constraint",
		Content: "---\nname: ack-constraint\ndescription: d\n---\nbody",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err = st.db.Exec(ctx, `UPDATE artifact SET findings_ack_digest='d' WHERE id=$1`, a.ID)
	if !isConstraintViolation(err, "artifact_findings_ack_complete") {
		t.Fatalf("digest with no actor/time must violate artifact_findings_ack_complete, got %v", err)
	}

	_, err = st.db.Exec(ctx, `UPDATE artifact SET findings_ack_by='alice' WHERE id=$1`, a.ID)
	if !isConstraintViolation(err, "artifact_findings_ack_complete") {
		t.Fatalf("actor with no digest/time must violate artifact_findings_ack_complete, got %v", err)
	}

	_, err = st.db.Exec(ctx, `UPDATE artifact SET findings_ack_at=now() WHERE id=$1`, a.ID)
	if !isConstraintViolation(err, "artifact_findings_ack_complete") {
		t.Fatalf("time with no digest/actor must violate artifact_findings_ack_complete, got %v", err)
	}

	if _, err := st.db.Exec(ctx, `UPDATE artifact SET findings_ack_digest='d', findings_ack_by='alice', findings_ack_at=now() WHERE id=$1`, a.ID); err != nil {
		t.Fatalf("all three together must be ACCEPTED, got %v", err)
	}
}

// TestWithdrawArtifactClearsFindingsDigestAndAcknowledgment extends
// WithdrawArtifact's existing "a withdrawn artifact is a true draft"
// contract (already applied to scan_findings itself, reset to '[]') to its
// digest and acknowledgment siblings. Without this, a withdrawn row could
// carry a non-empty scan_findings_digest while scan_findings reads '[]',
// which would make this file's own doc comment on ScanFindingsDigest ("a
// digest OVER ScanFindings") false for that row until the next submit
// happens to overwrite it.
func TestWithdrawArtifactClearsFindingsDigestAndAcknowledgment(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	a, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "ack-withdraw",
		Content: "---\nname: ack-withdraw\ndescription: d\n---\nbody",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.db.Exec(ctx, `UPDATE artifact SET scan_findings_digest='digest-1' WHERE id=$1`, a.ID); err != nil {
		t.Fatalf("seed scan_findings_digest: %v", err)
	}
	if _, err := st.SetArtifactFindingsAcknowledged(ctx, tn.ID, a.ID, "alice", "digest-1"); err != nil {
		t.Fatalf("set acknowledged: %v", err)
	}

	wd, err := st.WithdrawArtifact(ctx, tn.ID, a.ID)
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if wd.ScanFindingsDigest != "" || wd.FindingsAckDigest != "" || wd.FindingsAckBy != "" || wd.FindingsAckAt != nil {
		t.Fatalf("withdraw must clear the digest and the acknowledgment, got %+v", wd)
	}
}
