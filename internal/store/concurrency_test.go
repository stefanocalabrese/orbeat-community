package store

import (
	"context"
	"errors"
	"testing"
)

// TestRowVersionBumpsOnEveryArtifactUpdatePath is the "grep for the class" guard.
// Every statement that UPDATEs artifact must move row_version — otherwise a
// stale write sails through the precondition that exists to reject it. If a new
// mutating statement is added and this test is not extended, that statement is
// unguarded.
func TestRowVersionBumpsOnEveryArtifactUpdatePath(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	a, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "rv-paths",
		Content: "---\nname: rv-paths\ndescription: d\n---\nbody\n",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.RowVersion != 1 {
		t.Fatalf("fresh artifact row_version = %d, want 1", a.RowVersion)
	}

	prev := a.RowVersion
	step := func(what string, fn func() (Artifact, error)) {
		t.Helper()
		got, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got.RowVersion <= prev {
			t.Errorf("%s did not bump row_version: %d -> %d (a statement that "+
				"does not bump is a statement whose stale writes are accepted)",
				what, prev, got.RowVersion)
		}
		prev = got.RowVersion
	}

	step("UpdateArtifact", func() (Artifact, error) {
		a.Description = "changed"
		return s.UpdateArtifact(ctx, a, prev)
	})
	step("SetArtifactSubmitted", func() (Artifact, error) {
		return s.SetArtifactSubmitted(ctx, tn.ID, a.ID, "sub@example.com", []byte("[]"))
	})
	step("SetArtifactApproved", func() (Artifact, error) {
		out, _, err := s.SetArtifactApproved(ctx, tn.ID, a.ID, "app@example.com", 0)
		return out, err
	})
	step("SetArtifactSubmitted (2nd)", func() (Artifact, error) {
		return s.SetArtifactSubmitted(ctx, tn.ID, a.ID, "sub@example.com", []byte("[]"))
	})
	step("SetArtifactRejected", func() (Artifact, error) {
		return s.SetArtifactRejected(ctx, tn.ID, a.ID, "no")
	})
	step("WithdrawArtifact", func() (Artifact, error) {
		return s.WithdrawArtifact(ctx, tn.ID, a.ID)
	})
	// RollbackArtifact is Enterprise-only (artifact_revision.ee.go, not
	// registered in a generated Community tree — docs/specs/2026-08-19-
	// orbeat-community-repo-generation-design.md §4) and is covered
	// separately by TestRowVersionBumpsOnRollbackArtifact
	// (artifact_revision.ee_test.go), which is self-contained rather than a
	// continuation of this table so it can live in an .ee_test.go file on
	// its own.
}

// TestRowVersionIgnoresClientSuppliedValue pins that the trigger — not the
// caller — owns the column. If a future UPDATE sets row_version explicitly,
// the trigger must still win, or a client could pin the version and defeat
// every precondition.
func TestRowVersionIgnoresClientSuppliedValue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	m, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "rv-client", Transport: "http",
		EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.db.Exec(ctx,
		`UPDATE mcp_server SET description='x', row_version=999 WHERE id=$1`, m.ID); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.GetMCPServer(ctx, tn.ID, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RowVersion != m.RowVersion+1 {
		t.Errorf("row_version = %d, want %d — the trigger must override a "+
			"client-supplied value, not defer to it", got.RowVersion, m.RowVersion+1)
	}
}

// TestNoOpUpdateStillBumpsRowVersion documents a real consequence rather than
// asserting a nicety: an identical retry after a network timeout WILL 412 even
// though the first attempt succeeded. The portal copy must therefore say
// "reload to see the current state" and not imply someone else edited.
func TestNoOpUpdateStillBumpsRowVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	m, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "rv-noop", Transport: "http",
		EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.db.Exec(ctx,
		`UPDATE mcp_server SET description=description WHERE id=$1`, m.ID); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.GetMCPServer(ctx, tn.ID, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RowVersion != m.RowVersion+1 {
		t.Errorf("row_version = %d, want %d", got.RowVersion, m.RowVersion+1)
	}
}

// TestUpdateArtifactRejectsStaleVersion is the decisive lost-update case:
// UpdateArtifact's own row_version predicate is the ONLY place that can ever
// be pinned, because its only production caller (handleUpdateArtifact) holds
// a FOR UPDATE lock and always passes the version it JUST read — Task 7's
// handler-level precondition check runs (and rejects a stale client) BEFORE
// the store is ever called, so the store's own guard is unreachable from
// there. If this predicate were vacuous, this is the only test that would
// notice. The point of the assertion is not the error value — it's that the
// second, stale write never lands (last-write-wins is exactly what this
// slice exists to remove).
func TestUpdateArtifactRejectsStaleVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	a, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "rv-stale",
		Content: "---\nname: rv-stale\ndescription: d\n---\nV1\n",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	staleVersion := a.RowVersion // the version an earlier "reader" would have seen

	// A first, legitimate update using the version it read.
	a.Content = "---\nname: rv-stale\ndescription: d\n---\nV2\n"
	first, err := s.UpdateArtifact(ctx, a, staleVersion)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}

	// A second writer retries with the version it read BEFORE the first
	// update landed (e.g. a stale form, or a naive retry after a timeout).
	first.Content = "---\nname: rv-stale\ndescription: d\n---\nV3-SHOULD-NOT-LAND\n"
	if _, err := s.UpdateArtifact(ctx, first, staleVersion); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("stale UpdateArtifact: want ErrVersionMismatch, got %v", err)
	}

	got, err := s.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != "---\nname: rv-stale\ndescription: d\n---\nV2\n" {
		t.Fatalf("the stale write landed: content = %q, want the FIRST update's V2 content — "+
			"a rejected UpdateArtifact must never touch the row", got.Content)
	}
}

// TestUpdateMCPServerRejectsStaleVersion is TestUpdateArtifactRejectsStale-
// Version's counterpart for servers. Unlike the artifact path, this one IS
// also reachable via handleUpdateServer today (its Task-3 stopgap fetch
// happens to always pass the current version, same as the artifact handler),
// but it deserves its own direct store-level pin for the same reason: no
// planned task exercises a genuinely stale UpdateMCPServer call end to end
// before Task 6 lands.
func TestUpdateMCPServerRejectsStaleVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	m, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "rv-stale-srv", Transport: "http",
		EndpointOrCommand: "https://example.invalid/v1", Status: "active",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	staleVersion := m.RowVersion

	m.EndpointOrCommand = "https://example.invalid/v2"
	m.RowVersion = staleVersion
	first, err := s.UpdateMCPServer(ctx, m)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}

	// Retry with the version read BEFORE the first update landed.
	first.EndpointOrCommand = "https://example.invalid/v3-SHOULD-NOT-LAND"
	first.RowVersion = staleVersion
	if _, err := s.UpdateMCPServer(ctx, first); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("stale UpdateMCPServer: want ErrVersionMismatch, got %v", err)
	}

	got, err := s.GetMCPServer(ctx, tn.ID, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.EndpointOrCommand != "https://example.invalid/v2" {
		t.Fatalf("the stale write landed: endpoint = %q, want the FIRST update's v2 endpoint — "+
			"a rejected UpdateMCPServer must never touch the row", got.EndpointOrCommand)
	}
}

// TestArtifactSlimProjectionCarriesRealRowVersion pins spec §6's requirement
// of "an existing parity test PLUS a new explicit one". The existing parity
// coverage (TestArtifactPageSlimOmitsHeavyFieldsButKeepsApprovedFlag, and the
// red-proof in this slice's commit) only pins that artifactSlimCols has the
// same COLUMN COUNT as artifactCols — replacing row_version with a
// same-shaped constant (`1::bigint AS row_version`) would still satisfy a
// count check and slip through silently. This test pins the VALUE: the slim
// projection (the admin review-queue list row) must carry the artifact's
// REAL current row_version, not a placeholder — spec §10 has that list row's
// rowVersion fed straight into the Task-8 approve precondition, so a constant
// there would make every approve precondition wrong in the same direction.
func TestArtifactSlimProjectionCarriesRealRowVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	a, err := s.CreateArtifact(ctx, Artifact{
		TenantID: tn.ID, Type: "skill", Name: "slim-rv",
		Content: "---\nname: slim-rv\ndescription: d\n---\nbody\n",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a.Description = "moved off the fresh-row default"
	updated, err := s.UpdateArtifact(ctx, a, a.RowVersion)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.RowVersion == 1 {
		t.Fatalf("row_version is still 1 after an update — this fixture cannot distinguish " +
			"the real value from a `1::bigint` placeholder; fix the fixture, not the assertion")
	}

	full, err := s.GetArtifact(ctx, tn.ID, a.ID)
	if err != nil {
		t.Fatalf("get full: %v", err)
	}
	slim, err := s.ListArtifactsPage(ctx, tn.ID, ArtifactPageOpts{Limit: 100})
	if err != nil || len(slim) != 1 {
		t.Fatalf("slim list = %d rows, err=%v; want 1", len(slim), err)
	}
	if slim[0].RowVersion != full.RowVersion {
		t.Fatalf("slim row_version = %d, full row_version = %d — the slim projection must carry "+
			"the artifact's REAL row_version, not a same-count placeholder",
			slim[0].RowVersion, full.RowVersion)
	}
}
