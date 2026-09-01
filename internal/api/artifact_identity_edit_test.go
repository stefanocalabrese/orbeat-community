package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/marketplace"
)

// marketplaceSubagentPath is the relative path RenderArtifactsPlugin writes a
// subagent to, the sibling of marketplaceSkillPath
// (artifact_identity_distribution_test.go). Both are named here because a type
// change moves an artifact from one to the other, and the move is what a
// developer's disk actually sees.
func marketplaceSubagentPath(name string) string {
	return "plugins/" + marketplace.ArtifactsPluginName + "/agents/" + name + ".md"
}

// TestIdentityEditOnApprovedArtifactIsAcceptedAndDeferred is the gate for
// deleting the identity lock. handleUpdateArtifact used to refuse a name, type
// or visibility change while a live approved snapshot existed (400, "withdraw
// it first"); it now accepts one, drops the artifact back to draft, and leaves
// both distribution channels serving the identity that was approved.
//
// The 200 is the smallest half and the least interesting. A gate that only
// checked "the 400 became a 200" would pass on the version of this change that
// deleted the guard and nothing else, and that version is the defect: the edit
// would be accepted AND would reach every entitled machine at the next sync
// with no reviewer in the loop. So all three fields are read off the live row,
// all three off the snapshot, and both channels are read on either side of the
// approval.
//
// All three identity fields change in one PUT, deliberately. They fail
// differently (a rename moves the file, a type change moves it between
// directories, a visibility change moves it between channels) and the deleted
// condition was a three-way OR, so a single-field edit would leave two thirds
// of that condition restorable with this test still green.
//
// autoApprove is pinned off rather than left at New's default, which is
// edition-specific: a generated Community tree copies this ordinary _test.go
// verbatim and approves the edit inside the same transaction, so the pending
// state asserted below never exists there. Community's own behaviour is its
// own suite, TestAutoApprove*ReachesSyncImmediately in autoapprove_test.go.
func TestIdentityEditOnApprovedArtifactIsAcceptedAndDeferred(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.autoApprove = false

	role, err := st.CreateRole(ctx, tn.ID, "sec")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	id, _ := createEntitledArtifact(t, srv, st, tn, role.ID, map[string]any{
		"type": "skill", "name": "defer-before", "description": "d",
		"content":    "---\nname: defer-before\ndescription: d\n---\nV1 BODY",
		"visibility": "role",
	})
	approveArtifact(t, st, tn.ID, id)
	if name, content := syncPair(t, srv, tn, role.ID); name != "defer-before" || !strings.Contains(content, "V1 BODY") {
		t.Fatalf("precondition: Channel 2 must serve defer-before carrying V1, got %q / %q", name, content)
	}

	cur, err := st.GetArtifact(ctx, tn.ID, id)
	if err != nil {
		t.Fatalf("read artifact before the edit: %v", err)
	}
	rec := updateArtifactAs(t, srv, tn, id, cur.RowVersion, map[string]any{
		"type": "subagent", "name": "defer-after", "description": "d",
		"content":    "---\nname: defer-after\ndescription: d\n---\nV2 BODY",
		"visibility": "org",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("changing name, type and visibility on an approved artifact = %d, want 200: an identity "+
			"edit is deferred to its approval, never refused, and withdraw is not a Community route. body %s",
			rec.Code, rec.Body)
	}

	edited, err := st.GetArtifact(ctx, tn.ID, id)
	if err != nil {
		t.Fatalf("read artifact after the edit: %v", err)
	}
	if edited.Type != "subagent" || edited.Name != "defer-after" || edited.Visibility != "org" {
		t.Fatalf("the working copy did not take the edit, got %s/%s/%s",
			edited.Type, edited.Name, edited.Visibility)
	}
	if edited.ApprovalState != "draft" {
		t.Fatalf("approval_state = %q after an identity edit, want draft: the edit has to go back in "+
			"front of a reviewer, and an artifact left 'approved' would never be submittable", edited.ApprovalState)
	}
	if edited.ApprovedType != "skill" || edited.ApprovedName != "defer-before" || edited.ApprovedVisibility != "role" {
		t.Fatalf("the snapshot followed the edit, got %s/%s/%s: distribution reads exactly these three "+
			"columns, so a snapshot that tracks the working copy IS the unreviewed rename landing on "+
			"every machine", edited.ApprovedType, edited.ApprovedName, edited.ApprovedVisibility)
	}
	if !strings.Contains(edited.ApprovedContent, "V1 BODY") || strings.Contains(edited.ApprovedContent, "V2 BODY") {
		t.Fatalf("approved_content moved with the edit: %q", edited.ApprovedContent)
	}

	// THE CONSEQUENCE, on both channels. A developer keeps receiving the
	// artifact that was approved, under the name and the type it was approved
	// with, and the flip to org visibility has not happened yet either.
	rows := entitledSyncArtifacts(t, srv, tn, role.ID)
	if len(rows) != 1 {
		t.Fatalf("Channel 2 served %d artifacts after an unapproved identity edit, want exactly 1: %+v", len(rows), rows)
	}
	if rows[0]["name"] != "defer-before" || rows[0]["type"] != "skill" {
		t.Fatalf("Channel 2 served %v/%v, want skill/defer-before: the pair the client turns into a path "+
			"must be the approved one until the edit is approved", rows[0]["type"], rows[0]["name"])
	}
	if content, _ := rows[0]["content"].(string); !strings.Contains(content, "V1 BODY") || strings.Contains(content, "V2 BODY") {
		t.Fatalf("Channel 2 served the old identity carrying the NEW body %q: identity and content are one "+
			"snapshot and must never be observable apart", content)
	}
	// All THREE paths this artifact could occupy, not just the two the edit
	// asked for. Naming only the defer-after pair passes on a Channel 1 that
	// filters the live visibility while projecting the approved identity, which
	// publishes skills/defer-before/SKILL.md to everyone the moment the flip is
	// saved: the artifact reaches an audience nobody approved, under a name
	// that makes the leak look like the status quo.
	pending := renderedPluginPaths(t, st, tn.ID)
	for _, path := range []string{
		marketplaceSkillPath("defer-before"),
		marketplaceSkillPath("defer-after"),
		marketplaceSubagentPath("defer-after"),
	} {
		if slices.Contains(pending, path) {
			t.Fatalf("Channel 1 published %s on an unapproved visibility flip, got %v", path, pending)
		}
	}

	approveArtifact(t, st, tn.ID, id)

	// Approving promotes the three together: the artifact leaves Channel 2 and
	// lands on Channel 1 as a subagent under its new name.
	if rows := entitledSyncArtifacts(t, srv, tn, role.ID); len(rows) != 0 {
		t.Fatalf("an approved org-visibility artifact must leave Channel 2 entirely, got %+v", rows)
	}
	got := renderedPluginPaths(t, st, tn.ID)
	if !slices.Contains(got, marketplaceSubagentPath("defer-after")) {
		t.Fatalf("approving the edit must publish %s, got %v", marketplaceSubagentPath("defer-after"), got)
	}
	if slices.Contains(got, marketplaceSkillPath("defer-before")) {
		t.Fatalf("the path the edit abandoned survived the approval, got %v", got)
	}
	// A rejected identity edit is a different test; this one ends by proving
	// the artifact is coherent, live and snapshot agreeing on all three.
	final, err := st.GetArtifact(ctx, tn.ID, id)
	if err != nil {
		t.Fatalf("re-read artifact: %v", err)
	}
	if final.ApprovedType != final.Type || final.ApprovedName != final.Name || final.ApprovedVisibility != final.Visibility {
		t.Fatalf("approving left the pair split: live %s/%s/%s over snapshot %s/%s/%s",
			final.Type, final.Name, final.Visibility,
			final.ApprovedType, final.ApprovedName, final.ApprovedVisibility)
	}
}
