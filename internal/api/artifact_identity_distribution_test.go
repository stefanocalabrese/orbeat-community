package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/marketplace"
	"github.com/stefanocalabrese/orbeat-community/internal/publish"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// editArtifactViaAPI applies mutate to an artifact's editable fields and saves
// it through the real PUT /v1/admin/artifacts/{id} handler, returning the row
// as the database then holds it.
//
// It drove store.UpdateArtifact directly until the identity lock was deleted,
// because the handler used to refuse an identity edit while an approved
// snapshot was live and these tests exist to observe the state that refusal
// prevented. Now that the handler accepts one, the gates below run through the
// surface an admin actually uses, and restoring the guard fails them on their
// own preconditions instead of leaving them green on the locked code.
//
// The row_version is re-read here rather than taken from a caller-held copy:
// the 00013 trigger bumps it on every update, approval included, so a struct
// held across an approval carries a stale token and the PUT would answer 412.
func editArtifactViaAPI(t *testing.T, srv *Server, st *store.Store, tn store.Tenant, id string, mutate func(*store.Artifact)) store.Artifact {
	t.Helper()
	ctx := context.Background()
	cur, err := st.GetArtifact(ctx, tn.ID, id)
	if err != nil {
		t.Fatalf("read artifact before edit: %v", err)
	}
	mutate(&cur)
	rec := updateArtifactAs(t, srv, tn, id, cur.RowVersion, map[string]any{
		"type": cur.Type, "name": cur.Name, "description": cur.Description,
		"content": cur.Content, "memoryScope": cur.MemoryScope, "memorySeed": cur.MemorySeed,
		"version": cur.Version, "visibility": cur.Visibility,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /v1/admin/artifacts/%s = %d, want 200: an identity edit on an approved "+
			"artifact is accepted and deferred to its approval, never refused. body %s", id, rec.Code, rec.Body)
	}
	updated, err := st.GetArtifact(ctx, tn.ID, id)
	if err != nil {
		t.Fatalf("read artifact after edit: %v", err)
	}
	return updated
}

// renderedPluginPaths runs the real Channel-1 pipeline (publish.ActiveArtifacts
// -> marketplace.RenderArtifactsPlugin) and returns the sorted relative paths
// it would write.
//
// Paths, not struct fields. The path is what lands on a developer's disk, it is
// what a rename actually moves, and it is where a duplicate identity would
// silently collapse two artifacts into one map entry. Asserting on
// marketplace.Artifact.Name one layer earlier would pass on a renderer that
// ignored the name entirely.
func renderedPluginPaths(t *testing.T, st *store.Store, tenantID string) []string {
	t.Helper()
	arts, err := publish.ActiveArtifacts(context.Background(), st, tenantID)
	if err != nil {
		t.Fatalf("active artifacts: %v", err)
	}
	files, err := marketplace.RenderArtifactsPlugin(arts)
	if err != nil {
		t.Fatalf("render artifacts plugin: %v", err)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths
}

// syncPair returns the single artifact GET /v1/sync/artifacts serves to
// roleID, as the (name, content) pair the client writes to disk, and fails if
// the count is not exactly one. The two are read together on purpose: they are
// the pair this slice exists to keep in sync, so an assertion on either alone
// is an assertion on half the invariant.
func syncPair(t *testing.T, srv *Server, tn store.Tenant, roleID string) (name, content string) {
	t.Helper()
	rows := entitledSyncArtifacts(t, srv, tn, roleID)
	if len(rows) != 1 {
		t.Fatalf("sync served %d artifacts, want exactly 1: %+v", len(rows), rows)
	}
	name, _ = rows[0]["name"].(string)
	content, _ = rows[0]["content"].(string)
	return name, content
}

// TestSyncServesTheApprovedNameUntilTheRenameIsApproved is this slice's
// load-bearing gate: a rename takes effect for developers at APPROVAL, not at
// save. Between the two, GET /v1/sync/artifacts must keep serving the old name
// carrying the old bytes, and after the approval both must move together.
//
// The two preconditions are preconditions, never the assertion. A gate that
// checked only "the rename was accepted" would pass under every wrong
// implementation of this design, and its naive inverse is worse: assert only
// that sync serves the old name, leave the identity lock in place, and the
// rename never happens at all, sync serves the old name for the trivial
// reason, and the gate is green on the unfixed code. So this test proves the
// LIVE row moved (Name flipped, ApprovalState back to draft) and the SNAPSHOT
// did not (ApprovedName unchanged) before asserting anything about sync.
//
// Name and content are asserted as a pair in both halves. Sync used to project
// the live name next to the frozen body, so "serves the old name" and "serves
// the old bytes" were independently breakable, and the failure this design
// prevents is exactly the mismatch: a file written under one name holding
// another version's content.
//
// autoApprove is set false explicitly rather than left at New's default, which
// is edition-specific (false in this repo's build, true in a generated
// Community tree, which copies this ordinary _test.go verbatim). Under
// auto-approve there is no gap between saving and approving, so the pending
// state this test is about never exists.
func TestSyncServesTheApprovedNameUntilTheRenameIsApproved(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.autoApprove = false

	role, err := st.CreateRole(ctx, tn.ID, "sec")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	id, _ := createEntitledArtifact(t, srv, st, tn, role.ID, map[string]any{
		"type": "skill", "name": "identity-before", "description": "d",
		"content":    "---\nname: identity-before\ndescription: d\n---\nV1 BODY",
		"visibility": "role",
	})
	approveArtifact(t, st, tn.ID, id)

	if name, content := syncPair(t, srv, tn, role.ID); name != "identity-before" || !strings.Contains(content, "V1 BODY") {
		t.Fatalf("precondition: sync must serve the approved artifact as identity-before/V1, got %q / %q", name, content)
	}

	renamed := editArtifactViaAPI(t, srv, st, tn, id, func(a *store.Artifact) {
		a.Name = "identity-after"
		a.Content = "---\nname: identity-after\ndescription: d\n---\nV2 BODY"
	})
	// PRECONDITIONS. Without these three the assertion below is satisfied by
	// an implementation that simply refused the rename.
	if renamed.Name != "identity-after" {
		t.Fatalf("precondition: the live name did not move, got %q", renamed.Name)
	}
	if renamed.ApprovalState != "draft" {
		t.Fatalf("precondition: an identity edit must re-dirty the working copy, got approval_state %q", renamed.ApprovalState)
	}
	if renamed.ApprovedName != "identity-before" {
		t.Fatalf("precondition: the snapshot must still hold the old name, got approved_name %q", renamed.ApprovedName)
	}

	// THE GATE, first half.
	name, content := syncPair(t, srv, tn, role.ID)
	if name != "identity-before" {
		t.Fatalf("sync served %q before the rename was approved: distribution must read approved_name, "+
			"or a rename lands on every entitled machine with no reviewer in the loop", name)
	}
	if !strings.Contains(content, "V1 BODY") || strings.Contains(content, "V2 BODY") {
		t.Fatalf("sync served the old name carrying the NEW body %q: name and content are one snapshot "+
			"and must never be observable apart", content)
	}

	approveArtifact(t, st, tn.ID, id)

	// THE GATE, second half: approving promotes the pair, not just the bytes.
	name, content = syncPair(t, srv, tn, role.ID)
	if name != "identity-after" {
		t.Fatalf("sync served %q after approval, want identity-after: SetArtifactApproved must copy "+
			"name into approved_name, or an approved rename never reaches anyone", name)
	}
	if !strings.Contains(content, "V2 BODY") || strings.Contains(content, "V1 BODY") {
		t.Fatalf("sync served the new name carrying the OLD body %q", content)
	}
}

// TestSyncChannelFollowsApprovedVisibility covers the third locked field, and
// it is the one that is not a relocation but a change of DISTRIBUTION CHANNEL:
// role visibility ships through GET /v1/sync/artifacts (Channel 2), org
// visibility through the marketplace plugin (Channel 1), and flipping between
// them changes who receives the artifact without touching one
// artifact_entitlement row.
//
// Both channels are asserted at every step. "Channel 2 stopped serving it" on
// its own is also what a broken update that destroyed the artifact looks like,
// and "Channel 1 does not have it yet" is what an artifact that was never
// approved looks like.
//
// The entitlement row is deliberately left in place across the flip: it goes
// dormant rather than being deleted (store.ArtifactRoleGrants documents why),
// so its survival is what makes the artifact reappear on Channel 2 if the flip
// is ever reversed.
func TestSyncChannelFollowsApprovedVisibility(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.autoApprove = false

	role, err := st.CreateRole(ctx, tn.ID, "sec")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	id, _ := createEntitledArtifact(t, srv, st, tn, role.ID, map[string]any{
		"type": "skill", "name": "channel-pending", "description": "d",
		"content":    "---\nname: channel-pending\ndescription: d\n---\nROLE BODY",
		"visibility": "role",
	})
	approveArtifact(t, st, tn.ID, id)

	if name, _ := syncPair(t, srv, tn, role.ID); name != "channel-pending" {
		t.Fatalf("precondition: Channel 2 must serve the role-visible artifact, got %q", name)
	}
	if got := renderedPluginPaths(t, st, tn.ID); slices.Contains(got, marketplaceSkillPath("channel-pending")) {
		t.Fatalf("precondition: a role-visible artifact must not be on the Channel-1 plugin, got %v", got)
	}

	flipped := editArtifactViaAPI(t, srv, st, tn, id, func(a *store.Artifact) { a.Visibility = "org" })
	// PRECONDITIONS: the live flip really happened and the snapshot did not follow it.
	if flipped.Visibility != "org" {
		t.Fatalf("precondition: the live visibility did not move, got %q", flipped.Visibility)
	}
	if flipped.ApprovedVisibility != "role" {
		t.Fatalf("precondition: the snapshot must still say role, got approved_visibility %q", flipped.ApprovedVisibility)
	}

	// THE GATE: an unapproved flip moves nothing. The artifact stays on the
	// channel it was approved onto, and it does not appear on the other one.
	// The names, not syncPair, so a channel that served NOTHING reports the
	// reason rather than tripping a helper's arity check.
	if names := syncArtifactNames(t, srv, tn, []string{role.ID}); !slices.Equal(names, []string{"channel-pending"}) {
		t.Fatalf("Channel 2 served %v on an UNAPPROVED flip, want [channel-pending]: distribution must "+
			"filter on approved_visibility, or saving a visibility change relocates the artifact for "+
			"every developer before any reviewer sees it", names)
	}
	if got := renderedPluginPaths(t, st, tn.ID); slices.Contains(got, marketplaceSkillPath("channel-pending")) {
		t.Fatalf("Channel 1 picked the artifact up on an UNAPPROVED flip, got %v", got)
	}

	approveArtifact(t, st, tn.ID, id)

	// Approving moves it, and both sides are checked so "it left Channel 2" is
	// distinguishable from "it arrived on Channel 1".
	if rows := entitledSyncArtifacts(t, srv, tn, role.ID); len(rows) != 0 {
		t.Fatalf("an approved org-visibility artifact must leave Channel 2 entirely, got %+v", rows)
	}
	if got := renderedPluginPaths(t, st, tn.ID); !slices.Contains(got, marketplaceSkillPath("channel-pending")) {
		t.Fatalf("the artifact must ARRIVE on Channel 1 once the flip is approved, not just vanish from Channel 2, got %v", got)
	}
}

// TestMarketplacePathFollowsApprovedIdentity is the Channel-1 half of the
// load-bearing gate, asserted at the rendered file PATH, since that is what a
// rename moves on disk.
func TestMarketplacePathFollowsApprovedIdentity(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	// Explicit, not New's default, which is edition-specific: a generated
	// Community tree copies this ordinary _test.go verbatim and auto-approves
	// the edit inside the same transaction, so the pending state the gate
	// below observes would never exist there.
	srv.autoApprove = false

	// No entitlement: an org-visibility artifact reaches everyone through the
	// plugin, and granting one a role would be the mis-issued case a sibling
	// test already covers.
	art, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "plugin-before", Description: "d",
		Content: "---\nname: plugin-before\ndescription: d\n---\nV1 BODY", Visibility: "org",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	id := art.ID
	approveArtifact(t, st, tn.ID, id)

	oldPath, newPath := marketplaceSkillPath("plugin-before"), marketplaceSkillPath("plugin-after")
	got := renderedPluginPaths(t, st, tn.ID)
	if !slices.Contains(got, oldPath) {
		t.Fatalf("precondition: the approved artifact must be rendered at %s, got %v", oldPath, got)
	}

	renamed := editArtifactViaAPI(t, srv, st, tn, id, func(a *store.Artifact) {
		a.Name = "plugin-after"
		a.Content = "---\nname: plugin-after\ndescription: d\n---\nV2 BODY"
	})
	if renamed.Name != "plugin-after" || renamed.ApprovedName != "plugin-before" {
		t.Fatalf("precondition: want live plugin-after over snapshot plugin-before, got %q over %q",
			renamed.Name, renamed.ApprovedName)
	}

	// THE GATE: the published tree does not move the file until the rename is
	// approved. A `claude plugin update` between the two must be a no-op.
	got = renderedPluginPaths(t, st, tn.ID)
	if !slices.Contains(got, oldPath) || slices.Contains(got, newPath) {
		t.Fatalf("the plugin tree moved the file before the rename was approved: %v", got)
	}

	approveArtifact(t, st, tn.ID, id)

	got = renderedPluginPaths(t, st, tn.ID)
	if !slices.Contains(got, newPath) || slices.Contains(got, oldPath) {
		t.Fatalf("approving the rename must move the file and leave nothing behind at the old path: %v", got)
	}
}

// marketplaceSkillPath is the relative path RenderArtifactsPlugin writes a
// skill to, built from the exported plugin name so a change to either half
// fails the build rather than silently making these assertions vacuous.
func marketplaceSkillPath(name string) string {
	return "plugins/" + marketplace.ArtifactsPluginName + "/skills/" + name + "/SKILL.md"
}
