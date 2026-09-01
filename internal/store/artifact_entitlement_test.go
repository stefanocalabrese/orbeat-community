package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestArtifactEntitlement(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	role, _ := st.CreateRole(ctx, tn.ID, "sec")
	other, _ := st.CreateRole(ctx, tn.ID, "other")

	roleArt, _ := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "sec-rev",
		Content: "---\nname: sec-rev\ndescription: d\n---\nbody", Visibility: "role",
	})
	approveArtifact(t, st, tn.ID, roleArt.ID)
	// An org artifact must never surface via entitlement reads even if one existed.
	orgArt, _ := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "org-skill",
		Content: "---\nname: org-skill\ndescription: d\n---\nx",
	})
	approveArtifact(t, st, tn.ID, orgArt.ID)

	ent, err := st.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
		TenantID: tn.ID, RoleID: role.ID, ArtifactID: roleArt.ID,
	})
	if err != nil || ent.ID == "" {
		t.Fatalf("create entitlement: err=%v ent=%+v", err, ent)
	}

	got, err := st.ListEntitledArtifacts(ctx, tn.ID, []string{role.ID})
	if err != nil {
		t.Fatalf("list entitled: %v", err)
	}
	if len(got) != 1 || got[0].Name != "sec-rev" {
		t.Fatalf("want only entitled sec-rev, got %+v", got)
	}

	// SECURITY INVARIANT: even a (mis-issued) entitlement pointing at an org
	// artifact must NOT surface it: the approved_visibility='role' guard in
	// ListEntitledArtifacts excludes it, so an org artifact can never escape via sync.
	wrongEnt, err := st.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
		TenantID: tn.ID, RoleID: role.ID, ArtifactID: orgArt.ID,
	})
	if err != nil {
		t.Fatalf("create wrong entitlement: %v", err)
	}
	guarded, err := st.ListEntitledArtifacts(ctx, tn.ID, []string{role.ID})
	if err != nil {
		t.Fatalf("list entitled (guard): %v", err)
	}
	if len(guarded) != 1 || guarded[0].Name != "sec-rev" {
		t.Fatalf("approved_visibility='role' guard failed: org artifact leaked, got %+v", guarded)
	}
	// Remove the deliberately-wrong entitlement so later tenant counts stay clean.
	if err := st.DeleteArtifactEntitlement(ctx, tn.ID, wrongEnt.ID); err != nil {
		t.Fatalf("delete wrong entitlement: %v", err)
	}

	// A different role sees nothing; empty roleIDs sees nothing (fail-closed).
	if none, _ := st.ListEntitledArtifacts(ctx, tn.ID, []string{other.ID}); len(none) != 0 {
		t.Fatalf("other role should see nothing, got %+v", none)
	}
	if empty, _ := st.ListEntitledArtifacts(ctx, tn.ID, nil); len(empty) != 0 {
		t.Fatalf("empty roleIDs should see nothing, got %+v", empty)
	}

	if ok, _ := st.ArtifactExistsInTenant(ctx, tn.ID, roleArt.ID); !ok {
		t.Fatal("artifact should exist in tenant")
	}

	if ents, _ := st.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0, false); len(ents) != 1 {
		t.Fatalf("want 1 entitlement, got %+v", ents)
	}
	if err := st.DeleteArtifactEntitlement(ctx, tn.ID, ent.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ents, _ := st.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0, false); len(ents) != 0 {
		t.Fatalf("entitlement not deleted: %+v", ents)
	}
}

// TestDeleteArtifactEntitlementMalformedIDIsNotFound proves a non-UUID id is
// treated as ErrNotFound (mapping Postgres 22P02 invalid_text_representation),
// not surfaced as a raw driver error that would 500 at the API layer (audit
// B2c) — mirrors TestDeleteEntitlementMalformedIDIsNotFound.
func TestDeleteArtifactEntitlementMalformedIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	if err := st.DeleteArtifactEntitlement(ctx, tn.ID, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteArtifactEntitlement(bad id): want ErrNotFound, got %v", err)
	}
}

func approveArtifact(t *testing.T, s *Store, tenantID, id string) {
	t.Helper()
	approveArtifactKeeping(t, s, tenantID, id, 0)
}

// approveArtifactKeeping approves and prunes the artifact's revision chain down
// to the newest keep rows (keep <= 0 prunes nothing, which is what
// approveArtifact passes and what an unconfigured deployment does).
//
// It exists because keep is the ONLY way a store test can produce a chain whose
// MIN(revision_num) is not 1, and a fixture whose MIN is 1 cannot tell the
// aggregate apart from a literal. In production the value arrives as
// ORBEAT_ARTIFACT_REVISION_KEEP (internal/config/config.go), reaches
// api.Server.revisionKeep through SetArtifactRevisionKeep and is handed to
// SetArtifactApproved as this same argument.
func approveArtifactKeeping(t *testing.T, s *Store, tenantID, id string, keep int) {
	t.Helper()
	if err := s.InTx(context.Background(), func(tx *Store) error {
		if _, e := tx.GetArtifactForUpdate(context.Background(), tenantID, id); e != nil {
			return e
		}
		_, _, e := tx.SetArtifactApproved(context.Background(), tenantID, id, "approver", keep)
		return e
	}); err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
}

// TestBothDistributionQueriesShareOneScannedProjection pins the property the
// compiler cannot: ListEntitledArtifacts (Channel 2) and ListActiveOrgArtifacts
// (Channel 1) SELECT the same distArtifactCols and hand the result to the same
// POSITIONAL scan, scanDistArtifact. Until this slice the entitled query
// carried its own hand-copied, a.-prefixed duplicate of that column list, so a
// column added, dropped or reordered on one side and not the other was a
// runtime pgx scan error on a live sync rather than a build failure.
//
// Two assertions, because they fail on different mutants.
//
//  1. Every one of the projected fields carries its own value. A reorder
//     inside distArtifactCols (approved_name before approved_type, say) keeps
//     both queries agreeing with each other while giving every consumer a type
//     of "scan-parity" and a name of "subagent", which is how the artifact's
//     on-disk path gets built out of the wrong halves.
//  2. The two queries return the identical struct for the identical artifact.
//     This is the drift assertion: reintroduce a hand-copied list on either
//     side and the moment the shared const changes, one channel scans the old
//     columns and the two stop matching.
//
// The artifact is a subagent with a memory scope and a seed on purpose: those
// are the two projected columns nothing else in this test would touch, and
// they are the ones a truncated copy would drop off the end.
//
// It is APPROVED FOUR TIMES UNDER keep=2 before the Channel-2 read, and every
// part of that is load-bearing, one level up from the projection itself: a
// fixture carrying exactly one revision cannot tell MAX(revision_num) apart
// from a literal 1, and a fixture that never PRUNES cannot tell
// MIN(revision_num) apart from a literal 1 either, because an unpruned chain
// always starts at 1. Four approvals under keep=2 leave revisions 3 and 4
// alive, so Channel 2 observes MIN=3 and MAX=4: two different non-one numbers,
// which is what makes the two aggregates distinguishable from each other as
// well as from a constant. Pruning is not a contrived state, it is what
// ORBEAT_ARTIFACT_REVISION_KEEP does on any install that sets it, and it is
// the entire reason a pin has to be clamped rather than served literally.
//
// The flip to org then needs its own approval to take effect (distribution
// follows approved_visibility), which appends revision 5 and prunes 3, so
// Channel 1 observes MIN=4 and MAX=5. The two channels see the same artifact
// through deliberately DIFFERENT windows and the struct-equality assertion
// below pins that difference by value rather than normalising it away.
//
// min_revision_num is set to 7 by direct SQL: its admin route is a later task,
// and 7 is outside this fixture's revision range (3, 4, 5) on purpose. A
// projection that swapped the floor with either aggregate, or fed one of them
// into the other's scan position, disagrees with every expectation here rather
// than with none of them.
//
// The entitled query is the half that matters for the projection's shape.
// artifact_entitlement has an id column of its own, so a bare id in
// distArtifactCols is SQLSTATE 42702 there and legal in the org query: a gate
// that drove only ListActiveOrgArtifacts would be green on a Channel 2 that
// fails on every live sync.
func TestBothDistributionQueriesShareOneScannedProjection(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)
	role, _ := st.CreateRole(ctx, tn.ID, "sec")

	const body = "---\nname: scan-parity\ndescription: d\n---\nPARITY BODY"
	art, err := st.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "scan-parity", Description: "d",
		Content: body, MemoryScope: "project", MemorySeed: "PARITY SEED",
		Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if _, err := st.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
		TenantID: tn.ID, RoleID: role.ID, ArtifactID: art.ID,
	}); err != nil {
		t.Fatalf("entitle: %v", err)
	}
	// Four, under keep=2. See the doc comment: an unpruned chain cannot
	// falsify a hardcoded MIN of 1.
	const revisionKeep = 2
	for range 4 {
		approveArtifactKeeping(t, st, tn.ID, art.ID, revisionKeep)
	}
	const entRevision = 4 // MAX: one revision per approval, appended by SetArtifactApproved
	const entOldest = 3   // MIN: keep=2 leaves {3,4}, so deliberately not 1

	// The admin floor, written directly because PUT .../min-revision does not
	// exist yet. 7 is outside {3,4,5} on purpose (see the doc comment).
	const floor = 7
	if _, err := st.db.Exec(ctx,
		`UPDATE artifact SET min_revision_num=$1 WHERE id=$2`, floor, art.ID); err != nil {
		t.Fatalf("set min_revision_num: %v", err)
	}

	ent, err := st.ListEntitledArtifacts(ctx, tn.ID, []string{role.ID})
	if err != nil {
		t.Fatalf("list entitled: %v", err)
	}
	if len(ent) != 1 {
		t.Fatalf("list entitled = %d rows, want 1", len(ent))
	}
	want := Artifact{Type: "subagent", Name: "scan-parity", Content: body,
		MemoryScope: "project", MemorySeed: "PARITY SEED"}
	for _, c := range []struct{ field, got, want string }{
		{"Type", ent[0].Type, want.Type},
		{"Name", ent[0].Name, want.Name},
		{"Content", ent[0].Content, want.Content},
		{"MemoryScope", ent[0].MemoryScope, want.MemoryScope},
		{"MemorySeed", ent[0].MemorySeed, want.MemorySeed},
		// The id must be the ARTIFACT's, not "some uuid": project
		// artifact_entitlement's own id instead and every other field here
		// stays green while the registry key silently becomes a grant id.
		{"ID", ent[0].ID, art.ID},
	} {
		if c.got != c.want {
			t.Errorf("Channel 2 %s = %q, want %q: distArtifactCols and scanDistArtifact disagree on column order",
				c.field, c.got, c.want)
		}
	}
	// The three numbers the clamp reads, asserted separately because they fail
	// on different mutants: swapping two of them keeps all three present and
	// makes exactly two of these lines red.
	for _, c := range []struct {
		field     string
		got, want int
		why       string
	}{
		{"Revision", ent[0].Revision, entRevision,
			"the projection must report MAX(revision_num) for the artifact, not a constant"},
		{"OldestRevision", ent[0].OldestRevision, entOldest,
			"the projection must report MIN(revision_num) for the artifact; this chain was pruned, so 1 is wrong and MAX is wrong"},
		{"MinRevisionNum", ent[0].MinRevisionNum, floor,
			"the projection must report the artifact's own min_revision_num column, which is a floor an admin wrote and not an aggregate"},
	} {
		if c.got != c.want {
			t.Errorf("Channel 2 %s = %d, want %d: %s", c.field, c.got, c.want, c.why)
		}
	}

	// Same artifact, other channel. The flip is re-approved because
	// distribution now follows approved_visibility, so an unapproved flip
	// would leave ListActiveOrgArtifacts empty and this comparison vacuous.
	cur, err := st.GetArtifact(ctx, tn.ID, art.ID)
	if err != nil {
		t.Fatalf("re-read artifact: %v", err)
	}
	cur.Visibility = "org"
	if _, err := st.UpdateArtifact(ctx, cur, cur.RowVersion); err != nil {
		t.Fatalf("flip to org: %v", err)
	}
	approveArtifactKeeping(t, st, tn.ID, art.ID, revisionKeep)

	org, err := st.ListActiveOrgArtifacts(ctx, tn.ID)
	if err != nil {
		t.Fatalf("list org: %v", err)
	}
	if len(org) != 1 {
		t.Fatalf("list org = %d rows, want 1", len(org))
	}
	// Every field must match Channel 2's row EXCEPT the two aggregates, which
	// the flip's own approval advanced by one each: it appended revision 5 and
	// the keep=2 prune dropped 3, so the whole window slid. The floor did not
	// move, because UpdateArtifact does not write min_revision_num, which is
	// the property that makes it a separate route rather than a field on the
	// artifact update. Stating these expected values inside the comparison
	// rather than copying Channel 2's keeps the org side pinned too: a
	// projection that dropped an aggregate on this side reports 0 and one that
	// hardcodes reports 1, and both fail here rather than passing because the
	// two halves agree on being wrong.
	wantOrg := ent[0]
	wantOrg.Revision = entRevision + 1
	wantOrg.OldestRevision = entOldest + 1
	if !reflect.DeepEqual(org[0], wantOrg) {
		t.Fatalf("the two distribution queries returned different structs for one artifact:\nChannel 1 %+v\nwant      %+v",
			org[0], wantOrg)
	}
}

// TestArtifactExistsInTenantMalformedID is the artifactId half of the pair
// TestRoleExistsInTenantMalformedID (rbac_test.go) pins for roleId. Both ids
// arrive unvalidated from the SAME JSON body (handleCreateArtifactEntitlement),
// so a malformed value fails Postgres' uuid cast (SQLSTATE 22P02) instead of
// matching no row, and that has to read as "doesn't exist", (false, nil), not
// as an error.
//
// The asymmetry this closes was measured through the handler, not reasoned
// about: {"roleId":"not-a-uuid", ...} returned 400 "unknown roleId for tenant"
// while {"artifactId":"not-a-uuid", ...} on the same route returned 500
// "internal error", because errors.Is(err, pgx.ErrNoRows) does not match a
// 22P02 and internal/api/respond.go's fail() has no arm for that SQLSTATE.
func TestArtifactExistsInTenantMalformedID(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tn := mustTenant(t, st)

	if ok, err := st.ArtifactExistsInTenant(ctx, tn.ID, "not-a-uuid"); err != nil || ok {
		t.Fatalf("ArtifactExistsInTenant(malformed id): want (false, nil), got (%v, %v)", ok, err)
	}
}
