package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// grantedArtifact creates one role-visibility artifact plus a grant to each of
// roleNames, and returns the artifact id together with the entitlement ids in
// the same order.
func grantedArtifact(t *testing.T, s *Store, tenantID, artifactName string, roleNames ...string) (string, []string) {
	t.Helper()
	ctx := context.Background()
	art, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tenantID, Type: "skill", Name: artifactName,
		Content:    "---\nname: " + artifactName + "\ndescription: d\n---\nbody",
		Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create artifact %s: %v", artifactName, err)
	}
	var entIDs []string
	for _, rn := range roleNames {
		role, err := s.CreateRole(ctx, tenantID, rn)
		if err != nil {
			t.Fatalf("create role %s: %v", rn, err)
		}
		ent, err := s.CreateArtifactEntitlement(ctx, ArtifactEntitlement{
			TenantID: tenantID, RoleID: role.ID, ArtifactID: art.ID,
		})
		if err != nil {
			t.Fatalf("grant %s: %v", rn, err)
		}
		entIDs = append(entIDs, ent.ID)
	}
	return art.ID, entIDs
}

// TestArtifactRoleGrantsReportsRolesAndCount pins the read itself: the exact
// count, the role names in name order, and the two shapes a caller must never
// have to special-case, an empty (non-nil) list for an ungranted artifact and
// truncated=false when the cap did not bite.
//
// It also pins the ISOLATION that makes the number meaningful: a grant on a
// DIFFERENT artifact, and a grant in a different tenant, must not be counted.
func TestArtifactRoleGrantsReportsRolesAndCount(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	// Names deliberately out of insertion order, so a result that came back in
	// insertion order rather than name order fails.
	artID, _ := grantedArtifact(t, s, tn.ID, "graded", "zeta-team", "alpha-team")
	// A second artifact with its own grant: must not leak into artID's report.
	otherID, _ := grantedArtifact(t, s, tn.ID, "ungraded-neighbour", "other-team")

	got, err := s.ArtifactRoleGrants(ctx, tn.ID, artID)
	if err != nil {
		t.Fatalf("ArtifactRoleGrants: %v", err)
	}
	if got.Count != 2 {
		t.Errorf("Count = %d, want 2", got.Count)
	}
	if want := []string{"alpha-team", "zeta-team"}; !slices.Equal(got.RoleNames, want) {
		t.Errorf("RoleNames = %v, want %v (name order)", got.RoleNames, want)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false: two names is well under the cap")
	}

	// The neighbour reports only its own grant.
	neighbour, err := s.ArtifactRoleGrants(ctx, tn.ID, otherID)
	if err != nil {
		t.Fatalf("ArtifactRoleGrants(neighbour): %v", err)
	}
	if neighbour.Count != 1 || !slices.Equal(neighbour.RoleNames, []string{"other-team"}) {
		t.Errorf("neighbour = %+v, want count 1 [other-team]", neighbour)
	}

	// An ungranted artifact: zero, and an EMPTY slice rather than nil, so the
	// API layer's JSON is [] and never null.
	bare, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "bare",
		Content: "---\nname: bare\ndescription: d\n---\nx", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create bare artifact: %v", err)
	}
	none, err := s.ArtifactRoleGrants(ctx, tn.ID, bare.ID)
	if err != nil {
		t.Fatalf("ArtifactRoleGrants(bare): %v", err)
	}
	if none.Count != 0 || none.RoleNames == nil || len(none.RoleNames) != 0 {
		t.Errorf("bare = %+v, want count 0 with an empty non-nil RoleNames", none)
	}

	// Another tenant's grants are invisible even for the same artifact id: the
	// read is tenant-scoped in SQL, not in Go.
	other := mustTenant(t, s)
	cross, err := s.ArtifactRoleGrants(ctx, other.ID, artID)
	if err != nil {
		t.Fatalf("ArtifactRoleGrants(cross-tenant): %v", err)
	}
	if cross.Count != 0 {
		t.Errorf("cross-tenant Count = %d, want 0", cross.Count)
	}

	// A malformed id is ErrNotFound, not a raw 22P02 the API would 500 on
	// (v1.16.0's idCastNotFound rule, applied here through readGrantNames).
	if _, err := s.ArtifactRoleGrants(ctx, tn.ID, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ArtifactRoleGrants(bad id) = %v, want ErrNotFound", err)
	}
}

// TestArtifactRoleGrantsForUpdateWaitsOutConcurrentRevoke is the red-proof for
// the `FOR UPDATE OF ae` clause, and nothing else in the suite can fail for its
// absence.
//
// handleDeleteArtifactEntitlement takes no lock on the artifact row, so
// handleUpdateArtifact's FOR UPDATE on that row (which is what stops a
// concurrent grant INSERT) does nothing to stop a concurrent grant REVOKE. An
// unlocked read would therefore return a name whose grant is being deleted
// right now, the deleting transaction would commit, and the artifact.update
// audit record would name a role that holds nothing: a before-picture, which is
// exactly what this feature must not write.
//
// The interleaving is driven with two real transactions. A holder deletes one
// of two grants and keeps its transaction open; the reader runs the handler's
// own sequence (GetArtifactForUpdate, then the locking grants read) and must be
// observed BLOCKED before the holder commits. Remove the clause and this test
// fails twice over: the reader never blocks (waitForBlockedQuery times out) and
// the report says 2.
func TestArtifactRoleGrantsForUpdateWaitsOutConcurrentRevoke(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	artID, entIDs := grantedArtifact(t, s, tn.ID, "revoke-race", "doomed-team", "surviving-team")

	// The holder: a real, separate, still-open transaction that has revoked
	// the first grant but not yet committed. It deliberately does NOT touch
	// the artifact row, mirroring handleDeleteArtifactEntitlement.
	pgtx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer pgtx.Rollback(ctx) // no-op once committed below
	holder := &Store{db: pgtx}
	if err := holder.DeleteArtifactEntitlement(ctx, tn.ID, entIDs[0]); err != nil {
		t.Fatalf("holder revoke: %v", err)
	}

	type result struct {
		g   ArtifactRoleGrants
		err error
	}
	done := make(chan result, 1)
	go func() {
		var g ArtifactRoleGrants
		err := s.InTx(ctx, func(tx *Store) error {
			// The handler's exact order: lock the artifact row first, then
			// read the grants (internal/api/admin_artifacts.go).
			if _, e := tx.GetArtifactForUpdate(ctx, tn.ID, artID); e != nil {
				return e
			}
			var e error
			g, e = tx.ArtifactRoleGrantsForUpdate(ctx, tn.ID, artID)
			return e
		})
		done <- result{g, err}
	}()

	waitForBlockedQuery(t, s, "artifact_entitlement", 5*time.Second)

	if err := pgtx.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("locking grants read: %v", res.err)
		}
		if res.g.Count != 1 || !slices.Equal(res.g.RoleNames, []string{"surviving-team"}) {
			t.Errorf("report = %+v, want count 1 [surviving-team]: the grant revoked while the read waited was still counted",
				res.g)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the grants read did not return after the holder committed")
	}
}
