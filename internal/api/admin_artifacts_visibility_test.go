package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// syncArtifactNames drives GET /v1/sync/artifacts as a user holding roleIDs and
// returns the artifact names it served.
func syncArtifactNames(t *testing.T, srv *Server, tn store.Tenant, roleIDs []string) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1", RoleIDs: roleIDs}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncArtifacts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync artifacts = %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Artifacts []struct {
			Name string `json:"name"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode sync body: %v, raw=%s", err, rec.Body)
	}
	names := make([]string, 0, len(body.Artifacts))
	for _, a := range body.Artifacts {
		names = append(names, a.Name)
	}
	return names
}

// setVisibility flips one artifact's visibility through the store, preserving
// every other field, and returns the fresh row.
//
// Deliberately NOT through handleUpdateArtifact: this helper serves the test
// that pins what the DISTRIBUTION query does with dormant grants, so it wants
// the flip and its re-approval with no HTTP layer in between. The handler is
// exercised for the flip itself by TestSyncChannelFollowsApprovedVisibility
// (artifact_identity_distribution_test.go), which is where the deferral
// belongs.
func setVisibility(t *testing.T, s *store.Store, tenantID, id, visibility string) store.Artifact {
	t.Helper()
	ctx := context.Background()
	// Re-read rather than trusting a caller-held copy: the row_version trigger
	// (migration 00013) bumps on EVERY update, approval included, so a struct
	// captured before an approval carries a stale token and UpdateArtifact
	// would answer ErrVersionMismatch.
	a, err := s.GetArtifact(ctx, tenantID, id)
	if err != nil {
		t.Fatalf("read artifact before flip: %v", err)
	}
	a.Visibility = visibility
	updated, err := s.UpdateArtifact(ctx, a, a.RowVersion)
	if err != nil {
		t.Fatalf("set visibility %s: %v", visibility, err)
	}
	// UpdateArtifact resets approval_state to draft but leaves the approved
	// snapshot in place, so re-approving here keeps the artifact distributable
	// and is what an admin's own re-approval would do.
	approveArtifact(t, s, tenantID, updated.ID)
	got, err := s.GetArtifact(ctx, tenantID, updated.ID)
	if err != nil {
		t.Fatalf("re-read artifact: %v", err)
	}
	return got
}

// TestArtifactVisibilityFlipMakesGrantsDormantThenRevivesThem pins the
// behaviour this whole change exists to make legible, and which nothing pinned
// before: per-role grants are NOT deleted when an artifact is switched to org
// visibility. They stop being consulted (store.ListEntitledArtifacts filters on
// approved_visibility='role') and go dormant, and switching back to role revives
// every one of them, with nobody re-granting anything. setVisibility below
// re-approves after each flip, which is why the dormancy is observable here at
// all: since migration 00016 the channel follows the APPROVED visibility, so an
// unapproved flip changes nothing a developer receives.
//
// The assertion that carries the point is the entitlement IDS: the same rows,
// by primary key, serve the artifact again after the round trip. A test that
// only re-checked the sync output would pass just as well against an
// implementation that deleted the grants and had the admin re-create them.
func TestArtifactVisibilityFlipMakesGrantsDormantThenRevivesThem(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)

	platform, _ := st.CreateRole(ctx, tn.ID, "platform")
	security, _ := st.CreateRole(ctx, tn.ID, "security")
	art, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "dormant-skill",
		Content: "---\nname: dormant-skill\ndescription: d\n---\nbody", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	approveArtifact(t, st, tn.ID, art.ID)
	var granted []string
	for _, r := range []store.Role{platform, security} {
		ent, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
			TenantID: tn.ID, RoleID: r.ID, ArtifactID: art.ID,
		})
		if err != nil {
			t.Fatalf("grant to %s: %v", r.Name, err)
		}
		granted = append(granted, ent.ID)
	}
	slices.Sort(granted)
	roleIDs := []string{platform.ID, security.ID}

	// Live: both roles receive it.
	if got := syncArtifactNames(t, srv, tn, roleIDs); !slices.Equal(got, []string{"dormant-skill"}) {
		t.Fatalf("sync before the flip = %v, want [dormant-skill]", got)
	}

	// role -> org: the sync channel stops serving it (it now ships to everyone
	// through the Channel-1 plugin instead).
	art = setVisibility(t, st, tn.ID, art.ID, "org")
	if got := syncArtifactNames(t, srv, tn, roleIDs); len(got) != 0 {
		t.Fatalf("sync while org-visibility = %v, want none: an org artifact must never be served through entitlements", got)
	}
	// The grants are still there, untouched. This is the dormancy: invisible in
	// the distribution path, fully present in the table.
	dormant, err := st.ArtifactRoleGrants(ctx, tn.ID, art.ID)
	if err != nil {
		t.Fatalf("grants while org: %v", err)
	}
	if dormant.Count != 2 || !slices.Equal(dormant.RoleNames, []string{"platform", "security"}) {
		t.Fatalf("dormant grants = %+v, want count 2 [platform security]", dormant)
	}

	// org -> role: every dormant grant revives at once, with no re-granting.
	art = setVisibility(t, st, tn.ID, art.ID, "role")
	if got := syncArtifactNames(t, srv, tn, roleIDs); !slices.Equal(got, []string{"dormant-skill"}) {
		t.Fatalf("sync after the flip back = %v, want [dormant-skill]", got)
	}
	ents, err := st.ListArtifactEntitlementsPage(ctx, tn.ID, nil, 0, false)
	if err != nil {
		t.Fatalf("list entitlements: %v", err)
	}
	var surviving []string
	for _, e := range ents {
		if e.ArtifactID == art.ID {
			surviving = append(surviving, e.ID)
		}
	}
	slices.Sort(surviving)
	if !slices.Equal(surviving, granted) {
		t.Fatalf("entitlement ids after the round trip = %v, want the ORIGINAL %v: the revived grants must be the same rows, not new ones",
			surviving, granted)
	}

	// Each role individually still receives it, so what revived is the grants
	// themselves and not some tenant-wide fallback.
	for _, r := range []store.Role{platform, security} {
		if got := syncArtifactNames(t, srv, tn, []string{r.ID}); !slices.Equal(got, []string{"dormant-skill"}) {
			t.Errorf("sync for role %s = %v, want [dormant-skill]", r.Name, got)
		}
	}
	// A role that was never granted still receives nothing: reviving the
	// dormant grants must not widen the set.
	stranger, _ := st.CreateRole(ctx, tn.ID, "stranger")
	if got := syncArtifactNames(t, srv, tn, []string{stranger.ID}); len(got) != 0 {
		t.Errorf("sync for an ungranted role = %v, want none", got)
	}
}

// updateArtifactVisibility PUTs a full-replace update that changes only the
// visibility, and returns the recorder.
func updateArtifactVisibility(t *testing.T, srv *Server, tn store.Tenant, a store.Artifact, visibility string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	in := map[string]any{
		"type": a.Type, "name": a.Name, "description": a.Description,
		"content": a.Content, "version": a.Version, "visibility": visibility,
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/artifacts/"+a.ID, in, tn)
	req.SetPathValue("id", a.ID)
	req.Header.Set("If-Match", etag(a.RowVersion))
	srv.handleUpdateArtifact(rec, req)
	return rec
}

// decodeRoleGrants pulls the roleGrants object out of an artifact response body.
func decodeRoleGrants(t *testing.T, rec *httptest.ResponseRecorder) (present bool, count float64, roles []any, truncated bool) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v, raw=%s", err, rec.Body)
	}
	raw, ok := body["roleGrants"]
	if !ok {
		return false, 0, nil, false
	}
	g, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("roleGrants = %v, want an object", raw)
	}
	count, _ = g["count"].(float64)
	roles, _ = g["roles"].([]any)
	truncated, _ = g["truncated"].(bool)
	return true, count, roles, truncated
}

// TestArtifactUpdateAuditRecordsVisibilityFlipGrants is the legibility gate: a
// visibility flip changes who receives an artifact without touching a single
// entitlement row, so the artifact.update audit event has to say how many grants
// it put to sleep or woke up, and whose.
//
// Every assertion below is on the VALUE, never on the presence of a key. That
// distinction is not pedantry here: this repo has a recorded case (v1.25.0) of a
// test asserting a key existed and passing green on an empty value, so the bug
// shipped. Red-proven by deleting the metadata block from handleUpdateArtifact,
// which fails the roleGrantsAffected, roleGrantsEffect and roles assertions in
// both directions.
//
// The two roleGrantsEffect values asserted here are the DEFERRED ones. Since
// migration 00016 distribution reads approved_visibility, so a flip on a row
// whose snapshot has not caught up has not happened yet, and the value has to
// say so: "revived" on a pending flip would tell an operator alice already has
// the artifact back. Changing the VALUE rather than the doc comment is what
// makes that correction detectable at all, and this test is where it is
// detected: a rewording would have left it green. The sibling tests below
// cover the in-effect values.
func TestArtifactUpdateAuditRecordsVisibilityFlipGrants(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)
	// Pinned rather than left at New's edition-specific default: a generated
	// Community tree copies this ordinary _test.go verbatim and would
	// auto-approve each flip inside the same auditedTx, making every effect
	// below the immediate one. Pinning makes both trees run the same path and
	// assert the same values (maybeAutoApprove is shared and gated only on
	// this field). The immediate half is TestArtifactUpdateAuditVisibility
	// EffectIsImmediateUnderAutoApprove's job.
	srv.autoApprove = false

	platform, _ := st.CreateRole(ctx, tn.ID, "platform")
	security, _ := st.CreateRole(ctx, tn.ID, "security")
	// Left as a draft, so nothing is distributed at all and every flip below is
	// pending on a first approval. The approved-snapshot case, where the flip
	// is pending against a snapshot that is actively shipping, is
	// TestArtifactUpdateAuditVisibilityEffectOnAnApprovedArtifact's job.
	art, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "flipped-skill", Description: "d",
		Content: "---\nname: flipped-skill\ndescription: d\n---\nbody", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	for _, r := range []store.Role{platform, security} {
		if _, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
			TenantID: tn.ID, RoleID: r.ID, ArtifactID: art.ID,
		}); err != nil {
			t.Fatalf("grant to %s: %v", r.Name, err)
		}
	}

	// role -> org: two grants will go dormant, once the flip is approved.
	rec := updateArtifactVisibility(t, srv, tn, art, "org")
	if rec.Code != http.StatusOK {
		t.Fatalf("flip to org = %d, body %s", rec.Code, rec.Body)
	}
	ev := lastAuditEvent(t, st, tn.ID, "artifact.update")
	assertFlipMetadata(t, ev, "role", "org", "goes_dormant_on_approval", 2, []string{"platform", "security"})

	// The response carries the same numbers, so the caller learns what its own
	// write did without re-querying.
	present, count, roles, truncated := decodeRoleGrants(t, rec)
	if !present {
		t.Fatal("update response has no roleGrants: the caller is told nothing about what its flip did")
	}
	if count != 2 || len(roles) != 2 || roles[0] != "platform" || roles[1] != "security" || truncated {
		t.Errorf("response roleGrants = {count:%v roles:%v truncated:%v}, want {2 [platform security] false}", count, roles, truncated)
	}

	// org -> role: the same two grants revive, once the flip is approved.
	flipped, err := st.GetArtifact(ctx, tn.ID, art.ID)
	if err != nil {
		t.Fatalf("re-read artifact: %v", err)
	}
	rec = updateArtifactVisibility(t, srv, tn, flipped, "role")
	if rec.Code != http.StatusOK {
		t.Fatalf("flip back to role = %d, body %s", rec.Code, rec.Body)
	}
	ev = lastAuditEvent(t, st, tn.ID, "artifact.update")
	assertFlipMetadata(t, ev, "org", "role", "revives_on_approval", 2, []string{"platform", "security"})

	// An update that does NOT move the visibility writes none of those keys:
	// the record describes a transition, so it must not appear where there was
	// none. Without this, a handler that emitted the block unconditionally
	// (reporting "revived" on every content edit of a role artifact) would pass
	// every assertion above.
	current, err := st.GetArtifact(ctx, tn.ID, art.ID)
	if err != nil {
		t.Fatalf("re-read artifact: %v", err)
	}
	rec = updateArtifactVisibility(t, srv, tn, current, "role")
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op visibility update = %d, body %s", rec.Code, rec.Body)
	}
	ev = lastAuditEvent(t, st, tn.ID, "artifact.update")
	for _, k := range []string{"visibilityFrom", "visibilityTo", "roleGrantsAffected", "roleGrantsEffect", "roles"} {
		if v, ok := ev.Metadata[k]; ok {
			t.Errorf("metadata[%s] = %v on an update that did not change visibility, want the key absent", k, v)
		}
	}
	if ev.Metadata["name"] != "flipped-skill" {
		t.Errorf("metadata[name] = %v, want flipped-skill", ev.Metadata["name"])
	}
	// The response still reports the live grants: roleGrants is a fact about
	// the artifact, not about the transition, which is why it is not gated on
	// one.
	if present, count, _, _ := decodeRoleGrants(t, rec); !present || count != 2 {
		t.Errorf("roleGrants on a non-flip update = (present %v, count %v), want (true, 2)", present, count)
	}
}

// TestArtifactUpdateAuditVisibilityEffectOnAnApprovedArtifact is the half its
// sibling above cannot reach: a flip on an artifact that is actively being
// distributed, where "pending" means developers keep receiving it on the old
// channel meanwhile.
//
// It also pins the arm that is easiest to get wrong by branching on the edition
// instead of on the row. The second flip here goes BACK to the visibility the
// snapshot still carries, so there is nothing left pending and the honest value
// is the immediate one, even though auto-approve is off and no approval
// happened. A grantsEffect that keyed on s.autoApprove would report
// "revives_on_approval" there and be wrong: those grants never went dormant,
// because the flip away from role never reached distribution.
//
// The approved_visibility read before each assertion is the premise, not the
// gate. Without it a green here would not distinguish "the effect value tracks
// the snapshot" from "the snapshot happened to move too".
func TestArtifactUpdateAuditVisibilityEffectOnAnApprovedArtifact(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)
	srv.autoApprove = false // see the sibling above for why this is pinned

	platform, _ := st.CreateRole(ctx, tn.ID, "platform")
	art, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "shipping-skill", Description: "d",
		Content: "---\nname: shipping-skill\ndescription: d\n---\nbody", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if _, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
		TenantID: tn.ID, RoleID: platform.ID, ArtifactID: art.ID,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	approveArtifact(t, st, tn.ID, art.ID)

	// role -> org, deferred: the snapshot still says role, so the grants are
	// still live and every entitled developer still receives this artifact.
	approved, err := st.GetArtifact(ctx, tn.ID, art.ID)
	if err != nil {
		t.Fatalf("re-read artifact: %v", err)
	}
	if approved.ApprovedVisibility != "role" {
		t.Fatalf("precondition: approvedVisibility = %q, want role", approved.ApprovedVisibility)
	}
	if rec := updateArtifactVisibility(t, srv, tn, approved, "org"); rec.Code != http.StatusOK {
		t.Fatalf("flip to org = %d, body %s", rec.Code, rec.Body)
	}
	pending, err := st.GetArtifact(ctx, tn.ID, art.ID)
	if err != nil {
		t.Fatalf("re-read artifact: %v", err)
	}
	if pending.Visibility != "org" || pending.ApprovedVisibility != "role" {
		t.Fatalf("precondition: after the flip visibility/approvedVisibility = %q/%q, want org/role: "+
			"the flip has to be pending for the deferred value to be the true one",
			pending.Visibility, pending.ApprovedVisibility)
	}
	ev := lastAuditEvent(t, st, tn.ID, "artifact.update")
	assertFlipMetadata(t, ev, "role", "org", "goes_dormant_on_approval", 1, []string{"platform"})

	// org -> role, and this one IS in effect: approved_visibility never left
	// role, so the grants never went dormant and nothing is waiting on an
	// approval to bring them back.
	if rec := updateArtifactVisibility(t, srv, tn, pending, "role"); rec.Code != http.StatusOK {
		t.Fatalf("flip back to role = %d, body %s", rec.Code, rec.Body)
	}
	back, err := st.GetArtifact(ctx, tn.ID, art.ID)
	if err != nil {
		t.Fatalf("re-read artifact: %v", err)
	}
	if back.ApprovedVisibility != "role" {
		t.Fatalf("precondition: approvedVisibility = %q, want role (the snapshot must not have moved)",
			back.ApprovedVisibility)
	}
	ev = lastAuditEvent(t, st, tn.ID, "artifact.update")
	assertFlipMetadata(t, ev, "org", "role", "revived", 1, []string{"platform"})
}

// TestArtifactUpdateAuditVisibilityEffectIsImmediateUnderAutoApprove is the
// Community arm: tx.UpdateArtifact and s.maybeAutoApprove run in the same
// auditedTx, so approved_visibility moves with the live column and the flip is
// in effect before the audit row is written. The value has to be the plain
// "dormant" there, and it is the same grantsEffect call reaching it from the
// row rather than from an edition check.
func TestArtifactUpdateAuditVisibilityEffectIsImmediateUnderAutoApprove(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)
	srv.autoApprove = true

	platform, _ := st.CreateRole(ctx, tn.ID, "platform")
	art, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "auto-skill", Description: "d",
		Content: "---\nname: auto-skill\ndescription: d\n---\nbody", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if _, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
		TenantID: tn.ID, RoleID: platform.ID, ArtifactID: art.ID,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	if rec := updateArtifactVisibility(t, srv, tn, art, "org"); rec.Code != http.StatusOK {
		t.Fatalf("flip to org = %d, body %s", rec.Code, rec.Body)
	}
	flipped, err := st.GetArtifact(ctx, tn.ID, art.ID)
	if err != nil {
		t.Fatalf("re-read artifact: %v", err)
	}
	if flipped.ApprovedVisibility != "org" {
		t.Fatalf("precondition: approvedVisibility = %q, want org: auto-approve must have promoted the "+
			"flip in the same transaction, or the immediate value below is not the true one",
			flipped.ApprovedVisibility)
	}
	ev := lastAuditEvent(t, st, tn.ID, "artifact.update")
	assertFlipMetadata(t, ev, "role", "org", "dormant", 1, []string{"platform"})
}

// assertFlipMetadata checks every visibility-flip key by VALUE.
func assertFlipMetadata(t *testing.T, ev store.AuditEvent, from, to, effect string, count int, roles []string) {
	t.Helper()
	if ev.Decision != "allow" {
		t.Errorf("decision = %q, want allow", ev.Decision)
	}
	if ev.Metadata["visibilityFrom"] != from {
		t.Errorf("metadata[visibilityFrom] = %v, want %q", ev.Metadata["visibilityFrom"], from)
	}
	if ev.Metadata["visibilityTo"] != to {
		t.Errorf("metadata[visibilityTo] = %v, want %q", ev.Metadata["visibilityTo"], to)
	}
	if ev.Metadata["roleGrantsEffect"] != effect {
		t.Errorf("metadata[roleGrantsEffect] = %v, want %q", ev.Metadata["roleGrantsEffect"], effect)
	}
	// json numbers decode as float64.
	if n, ok := ev.Metadata["roleGrantsAffected"].(float64); !ok || int(n) != count {
		t.Errorf("metadata[roleGrantsAffected] = %v, want %d", ev.Metadata["roleGrantsAffected"], count)
	}
	got, ok := ev.Metadata["roles"].([]any)
	if !ok {
		t.Fatalf("metadata[roles] = %v, want an array of role names", ev.Metadata["roles"])
	}
	names := make([]string, 0, len(got))
	for _, r := range got {
		s, _ := r.(string)
		names = append(names, s)
	}
	if !slices.Equal(names, roles) {
		t.Errorf("metadata[roles] = %v, want %v", names, roles)
	}
	if trunc, ok := ev.Metadata["truncated"].(bool); !ok || trunc {
		t.Errorf("metadata[truncated] = %v, want false", ev.Metadata["truncated"])
	}
}

// lastAuditEvent returns the most recent audit event with the given action.
func lastAuditEvent(t *testing.T, st *store.Store, tenantID, action string) store.AuditEvent {
	t.Helper()
	evs, err := st.ListAuditEventsByTenant(context.Background(), tenantID, 50)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	for _, ev := range evs {
		if ev.Action == action {
			return ev
		}
	}
	t.Fatalf("no %s audit event among %+v", action, evs)
	return store.AuditEvent{}
}

// TestArtifactGetReportsRoleGrants pins the read the portal's edit form makes:
// the by-id route carries the grant count and role names, so the warning shown
// before a flip is server-derived. A client counting them itself from
// GET /v1/admin/artifact-entitlements would undercount on exactly the artifacts
// with the most grants, since that list is capped at 100 rows by default.
func TestArtifactGetReportsRoleGrants(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)

	platform, _ := st.CreateRole(ctx, tn.ID, "platform")
	art, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "read-grants",
		Content: "---\nname: read-grants\ndescription: d\n---\nbody", Visibility: "org",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if _, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
		TenantID: tn.ID, RoleID: platform.ID, ArtifactID: art.ID,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	get := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := adminReq(ctx, http.MethodGet, "/v1/admin/artifacts/"+id, nil, tn)
		req.SetPathValue("id", id)
		srv.handleGetArtifact(rec, req)
		return rec
	}

	rec := get(art.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d, body %s", rec.Code, rec.Body)
	}
	// The artifact is org-visibility, so this grant is DORMANT. Reporting it is
	// the whole point: this is the number an admin needs before flipping back.
	present, count, roles, truncated := decodeRoleGrants(t, rec)
	if !present || count != 1 || len(roles) != 1 || roles[0] != "platform" || truncated {
		t.Fatalf("roleGrants = (present %v, count %v, roles %v, truncated %v), want (true, 1, [platform], false)",
			present, count, roles, truncated)
	}

	// Zero grants is reported as zero, never as an absent key: a client must
	// not have to tell "no grants" from "not reported".
	bare, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "no-grants",
		Content: "---\nname: no-grants\ndescription: d\n---\nx", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create bare artifact: %v", err)
	}
	present, count, roles, _ = decodeRoleGrants(t, get(bare.ID))
	if !present || count != 0 || roles == nil || len(roles) != 0 {
		t.Fatalf("bare roleGrants = (present %v, count %v, roles %v), want (true, 0, [])", present, count, roles)
	}

	// The LIST route deliberately does not carry it (one query per row), and
	// its absence there is what makes the pointer field honest.
	listRec := httptest.NewRecorder()
	srv.handleListArtifacts(listRec, adminReq(ctx, http.MethodGet, "/v1/admin/artifacts", nil, tn))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list = %d, body %s", listRec.Code, listRec.Body)
	}
	var list struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Artifacts) == 0 {
		t.Fatal("list returned no artifacts")
	}
	for _, row := range list.Artifacts {
		if _, ok := row["roleGrants"]; ok {
			t.Errorf("list row %v carries roleGrants: the list is slim by design and a per-row count is one query per artifact", row["name"])
		}
	}
}

// waitForBlockedAPIQuery polls pg_stat_activity through its own connection
// until some backend is blocked on a lock while running a query containing
// substr. internal/store has the same helper for its own tests; this package
// cannot borrow it (unexported, different package) and cannot reach a pool
// through *store.Store either, so it opens a plain database/sql connection to
// the same test database.
func waitForBlockedAPIQuery(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	db, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatalf("open probe connection: %v", err)
	}
	defer db.Close()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(`
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query ILIKE '%' || $1 || '%'`, substr).Scan(&n); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for a blocked query matching %q", timeout, substr)
}

// TestArtifactUpdateGrantReportIsNotABeforePicture pins the ORDERING inside
// handleUpdateArtifact: the grants are read after GetArtifactForUpdate's row
// lock, never before it.
//
// This is v1.24.0's DeleteRole race, one table over, and that one was
// reproduced rather than reasoned about. handleCreateArtifactEntitlement takes
// ArtifactExistsInTenant's FOR SHARE lock on the artifact row and holds it
// across its own INSERT. Under this repo's READ COMMITTED isolation, a grants
// read taken BEFORE the FOR UPDATE lock completes immediately against the old
// state; the update then blocks behind that FOR SHARE holder, which can insert
// another grant and commit while it waits. The flip then applies to N+1 grants
// while the audit says N, on the one surface the feature exists to make legible.
//
// Two real concurrent transactions, no mocks: a holder takes the FOR SHARE lock
// and keeps it open, the update runs concurrently and is observed BLOCKED, and
// only then does the holder insert its grant and commit. The audit must name
// all three roles. Moving the grants read above GetArtifactForUpdate turns this
// red (it reports 2), which is the mutation that proves it.
func TestArtifactUpdateGrantReportIsNotABeforePicture(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)
	// Pinned for the same reason as the flip tests above: roleGrantsEffect now
	// reports whether the flip has reached distribution, so its value is
	// edition-dependent unless auto-approve is fixed. The subject here is the
	// grant COUNT and the role names, and pinning keeps the effect string from
	// making an unrelated assertion tier-dependent in a generated Community
	// tree, where this ordinary _test.go is copied verbatim.
	srv.autoApprove = false

	art, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "race-skill", Description: "d",
		Content: "---\nname: race-skill\ndescription: d\n---\nbody", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	// Two grants exist when the update starts reading.
	for _, name := range []string{"aaa-team", "bbb-team"} {
		role, err := st.CreateRole(ctx, tn.ID, name)
		if err != nil {
			t.Fatalf("create role %s: %v", name, err)
		}
		if _, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
			TenantID: tn.ID, RoleID: role.ID, ArtifactID: art.ID,
		}); err != nil {
			t.Fatalf("grant %s: %v", name, err)
		}
	}
	// The third role, granted mid-race by the holder.
	raceRole, err := st.CreateRole(ctx, tn.ID, "ccc-team")
	if err != nil {
		t.Fatalf("create race role: %v", err)
	}

	// The holder: a real open transaction doing exactly what
	// handleCreateArtifactEntitlement does, paused between the existence check
	// (which takes the FOR SHARE lock) and its INSERT.
	insertNow := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- st.InTx(ctx, func(tx *store.Store) error {
			ok, e := tx.ArtifactExistsInTenant(ctx, tn.ID, art.ID)
			if e != nil {
				return e
			}
			if !ok {
				return fmt.Errorf("artifact %s not found in tenant", art.ID)
			}
			<-insertNow
			_, e = tx.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
				TenantID: tn.ID, RoleID: raceRole.ID, ArtifactID: art.ID,
			})
			return e
		})
	}()
	// Let the holder actually take its lock before the update starts, so the
	// update is guaranteed to contend rather than racing past.
	waitForRowLockHeld(t, tn.ID, art.ID, 5*time.Second)

	updated := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		updated <- updateArtifactVisibility(t, srv, tn, art, "org")
	}()

	// The update must be genuinely blocked on the artifact row before the
	// holder adds its grant. Without that, this test would prove nothing about
	// ordering: the update could simply have finished first.
	waitForBlockedAPIQuery(t, "FROM artifact WHERE", 5*time.Second)
	close(insertNow)
	if err := <-holderDone; err != nil {
		t.Fatalf("holder tx: %v", err)
	}

	select {
	case rec := <-updated:
		if rec.Code != http.StatusOK {
			t.Fatalf("update = %d, body %s", rec.Code, rec.Body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the update did not return after the holder committed")
	}

	ev := lastAuditEvent(t, st, tn.ID, "artifact.update")
	assertFlipMetadata(t, ev, "role", "org", "goes_dormant_on_approval", 3,
		[]string{"aaa-team", "bbb-team", "ccc-team"})
}

// waitForRowLockHeld polls until a row lock on the artifact row is actually
// held, so a test can hand off to a contending transaction knowing the holder
// got there first. pg_locks does not expose which ROW is locked, so this looks
// for the tuple/transaction lock the FOR SHARE read takes on that table.
func waitForRowLockHeld(t *testing.T, tenantID, artifactID string, timeout time.Duration) {
	t.Helper()
	db, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatalf("open probe connection: %v", err)
	}
	defer db.Close()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var n int
		if err := db.QueryRow(`
			SELECT count(*) FROM pg_locks l
			JOIN pg_class c ON c.oid = l.relation
			WHERE c.relname = 'artifact' AND l.granted AND l.mode = 'RowShareLock'`).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for a held row lock on artifact %s (tenant %s)", timeout, artifactID, tenantID)
}
