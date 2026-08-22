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

	// submit
	sub, err := st.SetArtifactSubmitted(ctx, tn.ID, a.ID, "alice", []byte(`[{"rule":"x","severity":"warn"}]`))
	if err != nil || sub.ApprovalState != "pending" || sub.SubmittedBy != "alice" {
		t.Fatalf("submit: %v %+v", err, sub)
	}
	if string(sub.ScanFindings) != `[{"rule":"x","severity":"warn"}]` {
		t.Fatalf("findings not persisted: %q", sub.ScanFindings)
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
	resub, err := st.SetArtifactSubmitted(ctx, tn.ID, a.ID, "alice", []byte(`[]`))
	if err != nil || resub.ApprovalState != "pending" || resub.RejectReason != "" {
		t.Fatalf("resubmit must clear reject_reason: %v %+v", err, resub)
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
