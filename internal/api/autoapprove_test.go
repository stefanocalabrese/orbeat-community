package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// TestNewWiresAutoApproveFromTheEditionExtensionPoint pins the one statement
// about autoApprove that is true in BOTH editions: New reads the extension
// point (autoapprove.ee.go / autoapprove.community.go) rather than deciding
// for itself. The Enterprise VALUE assertion, communityAutoApprove() ==
// false, lives in autoapprove.ee_test.go, which the generator drops, because
// in a Community tree that statement is false by design.
//
// Stated honestly, because a weak assertion described as a strong one is
// worse than no assertion: in THIS build both sides of the comparison are
// false, so a New that never assigned the field at all would still pass here
// and only the .ee_test.go value pin would catch the drift. In a generated
// Community tree, where the extension point returns true, it is the decisive
// wiring check, and it is the only one that can live in a file that survives
// generation.
func TestNewWiresAutoApproveFromTheEditionExtensionPoint(t *testing.T) {
	srv := New(nil, nil, nil, nil, nil)
	if srv.autoApprove != communityAutoApprove() {
		t.Fatalf("New()'s autoApprove = %v, want communityAutoApprove()'s %v, New must read the "+
			"edition extension point, not decide for itself", srv.autoApprove, communityAutoApprove())
	}
}

// TestMaybeAutoApproveNoOpWhenDisabled proves the mechanism itself
// (maybeAutoApprove, admin_artifacts.go) does nothing when s.autoApprove is
// false: the artifact it is handed comes back byte-identical and no store
// write happens.
//
// autoApprove is set to false EXPLICITLY rather than left at New's default,
// which is what the test used to do: that default is edition-specific (false
// here, true in a generated Community tree), and this file is an ordinary
// _test.go that communitygen copies verbatim, so relying on it made a test
// about the shared mechanism fail for a reason that has nothing to do with
// the mechanism. Setting the field makes the pair with
// TestMaybeAutoApproveApprovesWhenEnabled a controlled experiment in both
// editions: identical setup, one variable flipped.
func TestMaybeAutoApproveNoOpWhenDisabled(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.autoApprove = false

	created, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "noop-skill", Content: "---\nname: noop-skill\ndescription: d\n---\nx",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var got store.Artifact
	if err := st.InTx(ctx, func(tx *store.Store) error {
		var e error
		got, e = srv.maybeAutoApprove(ctx, tx, tn.ID, created)
		return e
	}); err != nil {
		t.Fatalf("maybeAutoApprove: %v", err)
	}
	if !reflect.DeepEqual(got, created) {
		t.Fatalf("disabled maybeAutoApprove must return its input unchanged:\n got  %+v\n want %+v", got, created)
	}

	reread, err := st.GetArtifact(ctx, tn.ID, created.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if reread.HasApproved || reread.ApprovalState != "draft" {
		t.Fatalf("disabled maybeAutoApprove must not touch the store, got %+v", reread)
	}
}

// TestMaybeAutoApproveApprovesWhenEnabled proves the mechanism's real half:
// with s.autoApprove forced true (Community's shape; this repo's own build
// never sets it that way on its own, pinned by
// autoapprove.ee_test.go's TestCommunityAutoApproveIsFalseInThisBuild, which
// is Enterprise-only and so is absent from a generated Community tree), the
// artifact is approved, the snapshot
// matches the working copy, and approved_by is the system actor (never a
// human). The revision-history assertion (exactly one 'approval' revision
// appended) lives in autoapprove.ee_test.go, not here: it reads back via
// st.ListArtifactRevisions, which is Enterprise-only
// (artifact_revision.ee.go); this file is an ordinary _test.go, copied
// byte-for-byte into a generated Community tree by communitygen (see
// destPath, internal/communitygen/communitygen.go), so it must never
// reference a symbol that tree does not have.
func TestMaybeAutoApproveApprovesWhenEnabled(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.autoApprove = true

	created, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "enabled-skill",
		Content: "---\nname: enabled-skill\ndescription: d\n---\nBODY",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var got store.Artifact
	if err := st.InTx(ctx, func(tx *store.Store) error {
		var e error
		got, e = srv.maybeAutoApprove(ctx, tx, tn.ID, created)
		return e
	}); err != nil {
		t.Fatalf("maybeAutoApprove: %v", err)
	}
	if !got.HasApproved || got.ApprovalState != "approved" {
		t.Fatalf("enabled maybeAutoApprove must approve, got %+v", got)
	}
	if got.ApprovedContent != created.Content {
		t.Fatalf("approved snapshot must match the working copy: got %q, want %q", got.ApprovedContent, created.Content)
	}
	if got.ApprovedBy != communityAutoApproveActor {
		t.Fatalf("approved_by must be the system actor, got %q; recording a human as the approver of "+
			"content nobody approved would put a false name in an append-only governance record", got.ApprovedBy)
	}
}

// TestHandleCreateArtifactAutoApprovesWhenEnabled drives the real HTTP
// handler (not the mechanism directly) to prove maybeAutoApprove is actually
// WIRED into handleCreateArtifact, not merely present and unused; mirrors
// caps_test.go's TestHandleCreateServerEnforcesCapAtN1 for the same reason.
func TestHandleCreateArtifactAutoApprovesWhenEnabled(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)
	srv.autoApprove = true

	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", validSkillInput(), tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["approvalState"] != "approved" || got["approved"] != true {
		t.Fatalf("created artifact must be auto-approved, got %+v", got)
	}
	if got["approvedBy"] != communityAutoApproveActor {
		t.Fatalf("approvedBy = %v, want the system actor %q", got["approvedBy"], communityAutoApproveActor)
	}
}

// TestHandleUpdateArtifactReAutoApprovesWhenEnabled proves the update path
// (not just create): editing content while auto-approve is enabled must
// re-approve with the NEW content, or the edit would never take effect;
// UpdateArtifact always resets approval_state to draft, and the generated
// Community tree has no submit/approve workflow to move it back. The
// revision-count assertion (2 revisions: create's auto-approval + the
// edit's re-approval) lives in autoapprove.ee_test.go; see
// TestMaybeAutoApproveApprovesWhenEnabled's doc comment for why.
func TestHandleUpdateArtifactReAutoApprovesWhenEnabled(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)
	srv.autoApprove = true

	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", validSkillInput(), tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID         string `json:"id"`
		RowVersion int64  `json:"rowVersion"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	edited := validSkillInput()
	edited["content"] = "---\nname: fmt-skill\ndescription: formats code\n---\nEDITED BODY"
	urec := httptest.NewRecorder()
	ureq := adminReq(ctx, http.MethodPut, "/v1/admin/artifacts/"+created.ID, edited, tn)
	ureq.SetPathValue("id", created.ID)
	ureq.Header.Set("If-Match", etag(created.RowVersion))
	srv.handleUpdateArtifact(urec, ureq)
	if urec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body %s", urec.Code, urec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(urec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if got["approvalState"] != "approved" || got["approved"] != true {
		t.Fatalf("an edit under auto-approve must re-approve, not sit in draft forever, got %+v", got)
	}
	if content, _ := got["approvedContent"].(string); !strings.Contains(content, "EDITED BODY") {
		t.Fatalf("the approved snapshot must carry the EDITED content, got %q", content)
	}
}

// TestHandleCreateThenSyncServesAutoApprovedArtifact is this repo's own
// always-on proof of the gate the whole task exists for, "an artifact
// created through the API is served by GET /v1/sync/artifacts", driven
// through the real handlers with s.autoApprove forced true, exactly the
// value a generated Community tree's New() computes by default. It differs
// from the generated-tree gate in internal/communitygen in one respect: it
// proves the MECHANISM works, not that a real `communitygen.Generate` output
// actually WIRES autoApprove true by default (that residual gap, whether
// generation itself preserves the wiring, is what the communitygen-level
// gate closes; see this package's own report).
func TestHandleCreateThenSyncServesAutoApprovedArtifact(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.autoApprove = true

	role, err := st.CreateRole(ctx, tn.ID, "sec")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	in := map[string]any{
		"type": "skill", "name": "gate-skill", "description": "d",
		"content":    "---\nname: gate-skill\ndescription: d\n---\nBODY",
		"visibility": "role",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	if _, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{TenantID: tn.ID, RoleID: role.ID, ArtifactID: created.ID}); err != nil {
		t.Fatalf("entitle: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "dev1", RoleIDs: []string{role.ID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	srec := httptest.NewRecorder()
	srv.handleSyncArtifacts(srec, req)
	if srec.Code != http.StatusOK {
		t.Fatalf("sync status = %d, body %s", srec.Code, srec.Body)
	}
	var out struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(srec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sync: %v", err)
	}
	if len(out.Artifacts) != 1 || out.Artifacts[0]["name"] != "gate-skill" {
		t.Fatalf("the auto-approved artifact must be served by GET /v1/sync/artifacts, got %+v", out.Artifacts)
	}
}

// TestHandleCreateThenSyncEmptyWithoutAutoApprove red-proves the previous
// test's premise from the other side: WITHOUT auto-approve, the identical
// create+entitle sequence must serve NOTHING; pinning that auto-approve is
// what closes the gap, not an unrelated change to entitlement or sync
// filtering.
//
// autoApprove is set to false EXPLICITLY, and the test was renamed off
// "ByDefault" to say so. It used to lean on New's default, which is
// edition-specific (false here, true in a generated Community tree), so this
// file being copied verbatim by communitygen turned a controlled experiment
// into an assertion that a Community build serves nothing, which is the
// opposite of what that edition is for. Flipping exactly one variable
// against its sibling above is what the red-proof needs, and it is now valid
// in both editions.
func TestHandleCreateThenSyncEmptyWithoutAutoApprove(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.autoApprove = false

	role, err := st.CreateRole(ctx, tn.ID, "sec")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}

	in := map[string]any{
		"type": "skill", "name": "gate-skill", "description": "d",
		"content":    "---\nname: gate-skill\ndescription: d\n---\nBODY",
		"visibility": "role",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if _, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{TenantID: tn.ID, RoleID: role.ID, ArtifactID: created.ID}); err != nil {
		t.Fatalf("entitle: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "dev1", RoleIDs: []string{role.ID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	srec := httptest.NewRecorder()
	srv.handleSyncArtifacts(srec, req)
	if srec.Code != http.StatusOK {
		t.Fatalf("sync status = %d, body %s", srec.Code, srec.Body)
	}
	var out struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(srec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sync: %v", err)
	}
	if len(out.Artifacts) != 0 {
		t.Fatalf("without auto-approve, sync must serve nothing; this is the exact bug the task fixes: %+v", out.Artifacts)
	}
}

// entitledSyncArtifacts drives the real GET /v1/sync/artifacts handler as a
// normal user holding roleID and returns the decoded artifact rows. It reads
// through the Channel-2 read path (store.ListEntitledArtifacts), which is the
// surface every claim about what a developer machine actually receives has to
// be made against.
//
// Until migration 00016 that query joined the LIVE `type` and `name` to the
// FROZEN `approved_content`, so a rename not accompanied by a fresh snapshot
// shipped the new name carrying the old bytes, and the identity lock in
// admin_artifacts.go existed to refuse the rename outright. It now projects
// approved_type/approved_name alongside approved_content, so the pair it
// returns was approved together.
func entitledSyncArtifacts(t *testing.T, srv *Server, tn store.Tenant, roleID string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "dev1", RoleIDs: []string{roleID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncArtifacts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync status = %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode sync: %v", err)
	}
	return out.Artifacts
}

// createEntitledArtifact creates an artifact through the real create handler
// and grants it to roleID, returning its id and the rowVersion a caller must
// echo back as If-Match on the next update.
func createEntitledArtifact(t *testing.T, srv *Server, st *store.Store, tn store.Tenant, roleID string, in map[string]any) (string, int64) {
	t.Helper()
	ctx := context.Background()
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		ID         string `json:"id"`
		RowVersion int64  `json:"rowVersion"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if _, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
		TenantID: tn.ID, RoleID: roleID, ArtifactID: created.ID,
	}); err != nil {
		t.Fatalf("entitle: %v", err)
	}
	return created.ID, created.RowVersion
}

// updateArtifactAs drives the real update handler with an If-Match built from
// rowVersion and returns the recorder, so a caller can assert either a 200 body
// or a rejection.
func updateArtifactAs(t *testing.T, srv *Server, tn store.Tenant, id string, rowVersion int64, in map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := adminReq(context.Background(), http.MethodPut, "/v1/admin/artifacts/"+id, in, tn)
	req.SetPathValue("id", id)
	req.Header.Set("If-Match", etag(rowVersion))
	srv.handleUpdateArtifact(rec, req)
	return rec
}

// TestAutoApproveRenameReachesSyncImmediately is the Community regression gate
// for a rename: with auto-approve on, renaming an artifact that already has a
// live approved snapshot must succeed AND the sync channel must immediately
// serve the NEW name carrying the NEW content.
//
// IMMEDIATELY is the word that carries the point, and it is the half of this
// design Community does not get. Distribution reads approved_name next to
// approved_content (migration 00016), so an identity edit reaches developers
// when it is APPROVED, not when it is saved, and in Enterprise that is a
// second admin at a later time. Under auto-approve there is no later:
// tx.UpdateArtifact and s.maybeAutoApprove run in the SAME auditedTx, so the
// promotion commits with the edit and no reader can observe one without the
// other. Red-proved by suppressing the maybeAutoApprove call that follows the
// update, which leaves the snapshot at the old pair and this test reporting
// the OLD name, which is the pending state a Community admin has no route out
// of.
//
// The store is re-read at the end rather than trusting the handler's own
// response body: a stale snapshot is a property of the ROW, and the response
// is built from the same in-memory value the write produced.
func TestAutoApproveRenameReachesSyncImmediately(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.autoApprove = true

	role, err := st.CreateRole(ctx, tn.ID, "sec")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	id, rv := createEntitledArtifact(t, srv, st, tn, role.ID, map[string]any{
		"type": "skill", "name": "before-rename", "description": "d",
		"content":    "---\nname: before-rename\ndescription: d\n---\nV1 BODY",
		"visibility": "role",
	})

	before := entitledSyncArtifacts(t, srv, tn, role.ID)
	if len(before) != 1 || before[0]["name"] != "before-rename" {
		t.Fatalf("precondition: sync must serve the auto-approved artifact under its original name, got %+v", before)
	}

	rec := updateArtifactAs(t, srv, tn, id, rv, map[string]any{
		"type": "skill", "name": "after-rename", "description": "d",
		"content":    "---\nname: after-rename\ndescription: d\n---\nV2 BODY",
		"visibility": "role",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename under auto-approve = %d, want 200; an artifact's identity is editable in "+
			"every edition since the lock was deleted. body %s", rec.Code, rec.Body)
	}

	after := entitledSyncArtifacts(t, srv, tn, role.ID)
	if len(after) != 1 {
		t.Fatalf("sync must still serve exactly one artifact after the rename, got %+v", after)
	}
	if after[0]["name"] != "after-rename" {
		t.Fatalf("sync name = %v, want the NEW name after-rename", after[0]["name"])
	}
	content, _ := after[0]["content"].(string)
	if !strings.Contains(content, "V2 BODY") || strings.Contains(content, "V1 BODY") {
		t.Fatalf("sync served the new name with STALE content %q: sync serves approved_name next to "+
			"approved_content, so seeing the new name here means the snapshot was refreshed, and the "+
			"body must be the new one too. Auto-approve gets that for free only because the update "+
			"and its re-approval commit in one transaction", content)
	}

	reread, err := st.GetArtifact(ctx, tn.ID, id)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if reread.Name != "after-rename" || reread.ApprovedContent != reread.Content {
		t.Fatalf("the approved snapshot must not be stale after a rename: name %q, content %q, approved %q",
			reread.Name, reread.Content, reread.ApprovedContent)
	}
}

// TestAutoApproveTypeChangeReachesSyncImmediately covers the second of the
// three identity fields on its own, because a type change is not a rename:
// the sync client derives an artifact's path from type AND name
// (fileBackedTypes, internal/syncclient/reconcile.go), so skill -> subagent
// moves the file from skills/<name>/SKILL.md to agents/<name>.md. What this
// package owns is the payload that drives that move, so the assertion is that
// the sync row's `type` flips and its content is the newly approved body, not
// the snapshot taken while it was still a skill.
func TestAutoApproveTypeChangeReachesSyncImmediately(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.autoApprove = true

	role, err := st.CreateRole(ctx, tn.ID, "sec")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	id, rv := createEntitledArtifact(t, srv, st, tn, role.ID, map[string]any{
		"type": "skill", "name": "type-move", "description": "d",
		"content":    "---\nname: type-move\ndescription: d\n---\nSKILL BODY",
		"visibility": "role",
	})
	before := entitledSyncArtifacts(t, srv, tn, role.ID)
	if len(before) != 1 || before[0]["type"] != "skill" {
		t.Fatalf("precondition: sync must serve the artifact as a skill, got %+v", before)
	}

	rec := updateArtifactAs(t, srv, tn, id, rv, map[string]any{
		"type": "subagent", "name": "type-move", "description": "d",
		"content":    "---\nname: type-move\ndescription: d\n---\nSUBAGENT BODY",
		"visibility": "role",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("type change under auto-approve = %d, want 200, body %s", rec.Code, rec.Body)
	}

	after := entitledSyncArtifacts(t, srv, tn, role.ID)
	if len(after) != 1 || after[0]["type"] != "subagent" {
		t.Fatalf("sync must serve the artifact under its NEW type, got %+v", after)
	}
	content, _ := after[0]["content"].(string)
	if !strings.Contains(content, "SUBAGENT BODY") {
		t.Fatalf("sync served the new type with the pre-change snapshot %q; the client would write "+
			"agents/type-move.md holding the skill's body", content)
	}
}

// TestAutoApproveVisibilityChangeSwitchesChannelImmediately covers the third
// identity field, which is the one that is not a relocation but a change of
// DISTRIBUTION CHANNEL: role visibility is served by
// GET /v1/sync/artifacts (Channel 2), org visibility by the marketplace plugin
// (Channel 1, store.ListActiveOrgArtifacts). Both sides are asserted, because
// "sync no longer serves it" on its own is also what a broken update that
// simply destroyed the artifact would look like.
//
// The role's artifact_entitlement row is deliberately left in place: it is
// inert while the APPROVED visibility is 'org' (ListEntitledArtifacts filters
// on approved_visibility since migration 00016) and becomes live again if the
// artifact is flipped back, which is the behaviour a Community admin
// correcting a mistake depends on.
func TestAutoApproveVisibilityChangeSwitchesChannelImmediately(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	srv.autoApprove = true

	role, err := st.CreateRole(ctx, tn.ID, "sec")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	id, rv := createEntitledArtifact(t, srv, st, tn, role.ID, map[string]any{
		"type": "skill", "name": "channel-move", "description": "d",
		"content":    "---\nname: channel-move\ndescription: d\n---\nROLE BODY",
		"visibility": "role",
	})
	if got := entitledSyncArtifacts(t, srv, tn, role.ID); len(got) != 1 {
		t.Fatalf("precondition: sync must serve the role-visible artifact, got %+v", got)
	}
	orgBefore, err := st.ListActiveOrgArtifacts(ctx, tn.ID)
	if err != nil {
		t.Fatalf("list org artifacts: %v", err)
	}
	if len(orgBefore) != 0 {
		t.Fatalf("precondition: a role-visible artifact must not be on the Channel-1 plugin, got %+v", orgBefore)
	}

	rec := updateArtifactAs(t, srv, tn, id, rv, map[string]any{
		"type": "skill", "name": "channel-move", "description": "d",
		"content":    "---\nname: channel-move\ndescription: d\n---\nORG BODY",
		"visibility": "org",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("visibility change under auto-approve = %d, want 200, body %s", rec.Code, rec.Body)
	}

	if got := entitledSyncArtifacts(t, srv, tn, role.ID); len(got) != 0 {
		t.Fatalf("an org-visibility artifact must leave Channel 2 entirely, got %+v", got)
	}
	orgAfter, err := st.ListActiveOrgArtifacts(ctx, tn.ID)
	if err != nil {
		t.Fatalf("list org artifacts: %v", err)
	}
	if len(orgAfter) != 1 || orgAfter[0].Name != "channel-move" {
		t.Fatalf("the artifact must ARRIVE on Channel 1, not just vanish from Channel 2, got %+v", orgAfter)
	}
	if !strings.Contains(orgAfter[0].Content, "ORG BODY") {
		t.Fatalf("Channel 1 served the pre-change snapshot %q; ListActiveOrgArtifacts reads "+
			"approved_content, so the flip is only safe because the re-approval shares the update's "+
			"transaction", orgAfter[0].Content)
	}
}
