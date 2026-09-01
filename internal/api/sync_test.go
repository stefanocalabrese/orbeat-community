package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// approveArtifact drives an artifact to the approved state so distribution
// (which now serves only the approved snapshot) will hand it out.
func approveArtifact(t *testing.T, s *store.Store, tenantID, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.InTx(ctx, func(tx *store.Store) error {
		if _, e := tx.GetArtifactForUpdate(ctx, tenantID, id); e != nil {
			return e
		}
		_, _, e := tx.SetArtifactApproved(ctx, tenantID, id, "approver", 0)
		return e
	}); err != nil {
		t.Fatalf("approve %s: %v", id, err)
	}
}

func TestHandleSyncConfigReturnsGatewayURL(t *testing.T) {
	srv := New(nil, nil, nil, nil, nil)
	srv.SetGatewayURL("https://gw.example.com")

	req := httptest.NewRequest(http.MethodGet, "/v1/sync/config", nil)
	// handleSyncConfig calls s.resolved, which fails closed (500) without a
	// resolved identity in context — inject one like the sibling sync tests do.
	rc := authz.ResolvedContext{TenantID: "t1", UserID: "u1"}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		GatewayURL string `json:"gateway_url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.GatewayURL != "https://gw.example.com" {
		t.Fatalf("gateway_url = %q", body.GatewayURL)
	}
}

func TestSyncArtifactsReturnsOnlyEntitledRoleArtifacts(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, _ := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("sync-%d", time.Now().UnixNano()))
	role, _ := s.CreateRole(ctx, tn.ID, "sec")

	// Entitled role subagent (memory scope → must be injected into returned content).
	roleArt, _ := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "sec-rev",
		Content: "---\nname: sec-rev\ndescription: d\n---\nbody", MemoryScope: "project",
		Visibility: "role",
	})
	_, _ = s.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{TenantID: tn.ID, RoleID: role.ID, ArtifactID: roleArt.ID})
	approveArtifact(t, s, tn.ID, roleArt.ID)
	// Org artifact: must NOT be returned by sync.
	_, _ = s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "org-skill",
		Content: "---\nname: org-skill\ndescription: d\n---\nx",
	})
	// Role artifact the caller is NOT entitled to: must NOT be returned.
	_, _ = s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "other-role-skill",
		Content: "---\nname: other-role-skill\ndescription: d\n---\nx", Visibility: "role",
	})

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1", RoleIDs: []string{role.ID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncArtifacts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Artifacts) != 1 || body.Artifacts[0]["name"] != "sec-rev" {
		t.Fatalf("want only entitled sec-rev, got %+v", body.Artifacts)
	}
	content, _ := body.Artifacts[0]["content"].(string)
	if !strings.Contains(content, "memory: project") {
		t.Fatalf("subagent memory not injected in sync content: %q", content)
	}
}

func TestSyncArtifactsEmptyForNoRoles(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, _ := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("sync-empty-%d", time.Now().UnixNano()))

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1", RoleIDs: nil}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncArtifacts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var body struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Artifacts) != 0 {
		t.Fatalf("no roles → no artifacts, got %+v", body.Artifacts)
	}
}

func TestSyncArtifactsCarriesSeedOnlyForSeededUserProjectSubagents(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, _ := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("sync-seed-%d", time.Now().UnixNano()))
	role, _ := s.CreateRole(ctx, tn.ID, "sec")

	seeded, _ := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "seeded",
		Content:     "---\nname: seeded\ndescription: d\n---\nbody",
		MemoryScope: "user", MemorySeed: "## Org standards\nseed body",
		Visibility: "role",
	})
	unseeded, _ := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "unseeded",
		Content:     "---\nname: unseeded\ndescription: d\n---\nbody",
		MemoryScope: "local", Visibility: "role",
	})
	// Not reachable via the admin API (no CHECK covers local+seed), but the store
	// allows it — the Go-level gate in handleSyncArtifacts must still fail closed
	// on the scope clause alone, independent of the emptiness check above.
	sneaky, err := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "sneaky",
		Content:     "---\nname: sneaky\ndescription: d\n---\nbody",
		MemoryScope: "local", MemorySeed: "x", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create sneaky: %v", err)
	}
	for _, id := range []string{seeded.ID, unseeded.ID, sneaky.ID} {
		_, _ = s.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{TenantID: tn.ID, RoleID: role.ID, ArtifactID: id})
		approveArtifact(t, s, tn.ID, id)
	}

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1", RoleIDs: []string{role.ID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncArtifacts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]map[string]any{}
	for _, a := range body.Artifacts {
		byName[a["name"].(string)] = a
	}
	if len(byName) != 3 {
		t.Fatalf("want 3 artifacts in response, got %v", body.Artifacts)
	}
	got := byName["seeded"]
	if got["memoryScope"] != "user" || got["memorySeed"] != "## Org standards\nseed body" {
		t.Fatalf("seeded artifact must carry scope+seed: %+v", got)
	}
	// omitempty: the fields must be ABSENT (not empty strings) on the unseeded
	// and sneaky (local-scope-but-seeded) ones.
	for _, name := range []string{"unseeded", "sneaky"} {
		for _, k := range []string{"memoryScope", "memorySeed"} {
			if _, present := byName[name][k]; present {
				t.Fatalf("%s artifact must omit %s: %+v", name, k, byName[name])
			}
		}
	}
}

func TestSyncServesApprovedSnapshotOnly(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, _ := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("sync-appr-%d", time.Now().UnixNano()))
	role, _ := s.CreateRole(ctx, tn.ID, "sec")

	// Unapproved role artifact — must NOT appear.
	draft, _ := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "draftsub",
		Content: "---\nname: draftsub\ndescription: d\n---\nWORK", MemoryScope: "user", Visibility: "role",
	})
	// Approved role artifact, then edited — must appear with the APPROVED content.
	appr, _ := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "apprsub",
		Content: "---\nname: apprsub\ndescription: d\n---\nAPPROVED", MemoryScope: "user",
		MemorySeed: "SEED-APPROVED", Visibility: "role",
	})
	for _, id := range []string{draft.ID, appr.ID} {
		_, _ = s.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{TenantID: tn.ID, RoleID: role.ID, ArtifactID: id})
	}
	approveArtifact(t, s, tn.ID, appr.ID)
	// edit working copy after approval — snapshot must win. approveArtifact
	// bumped row_version without touching this local appr, so re-fetch the
	// current version rather than reuse appr's now-stale one.
	current, err := s.GetArtifact(ctx, tn.ID, appr.ID)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	appr.Content = "---\nname: apprsub\ndescription: d\n---\nEDITED"
	appr.MemorySeed = "SEED-EDITED"
	if _, err := s.UpdateArtifact(ctx, appr, current.RowVersion); err != nil {
		t.Fatalf("update: %v", err)
	}

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1", RoleIDs: []string{role.ID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncArtifacts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Artifacts) != 1 {
		t.Fatalf("only the approved artifact must appear: %+v", body.Artifacts)
	}
	a := body.Artifacts[0]
	if a["name"] != "apprsub" || a["memorySeed"] != "SEED-APPROVED" {
		t.Fatalf("must serve approved snapshot: %+v", a)
	}
	if got, _ := a["content"].(string); !strings.Contains(got, "APPROVED") || strings.Contains(got, "EDITED") {
		t.Fatalf("content must be approved snapshot: %q", got)
	}
}

// TestSyncArtifactsCarryIDAndRevisionUnconditionally pins the prerequisite for
// any deployment registry: a machine cannot report a version it was never
// told. Until this slice, GET /v1/sync/artifacts handed a developer bytes, a
// type and a name and nothing that identifies WHICH artifact or WHICH version
// those bytes are, so the only handle the client held on what it had written
// was the file path.
//
// Two properties, and the store-level parity gate
// (store.TestBothDistributionQueriesShareOneScannedProjection) proves neither
// of them:
//
//   - The values survive the store -> DTO -> JSON hop. handleSyncArtifacts
//     builds syncArtifactDTO field by field, so a correct projection reaches a
//     response missing both fields if that literal forgets them, and no store
//     test can see it.
//   - The keys are actually in the wire body. revision carries omitempty, so a
//     projection that quietly reported 0 would produce a response with no
//     revision key at all rather than a wrong number, which is why the
//     assertion is on presence AND value.
//
// UNCONDITIONAL is what the file this test lives in proves: an ordinary
// _test.go, copied verbatim into a generated Community tree by
// internal/communitygen, where TestGenerateProducesTestableTree runs it on
// every `go test ./...`. There is no edition in which these fields are absent.
//
// Approved THREE times on purpose. A fixture with one revision cannot tell
// MAX(revision_num) from a literal 1, and the store gate already uses 2, so a
// projection that had been mutated to satisfy that one still fails here.
func TestSyncArtifactsCarryIDAndRevisionUnconditionally(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, _ := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("sync-ident-%d", time.Now().UnixNano()))
	role, _ := s.CreateRole(ctx, tn.ID, "sec")

	art, err := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "ident-skill",
		Content: "---\nname: ident-skill\ndescription: d\n---\nBODY", Visibility: "role",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if _, err := s.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{
		TenantID: tn.ID, RoleID: role.ID, ArtifactID: art.ID,
	}); err != nil {
		t.Fatalf("entitle: %v", err)
	}
	const wantRevision = 3 // one revision appended per approval
	for i := 0; i < wantRevision; i++ {
		approveArtifact(t, s, tn.ID, art.ID)
	}

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1", RoleIDs: []string{role.ID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncArtifacts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Artifacts) != 1 {
		t.Fatalf("want exactly the entitled artifact, got %+v", body.Artifacts)
	}
	got := body.Artifacts[0]

	rawID, ok := got["id"]
	if !ok {
		t.Fatalf("the response carries no id key at all: %+v", got)
	}
	if rawID != art.ID {
		t.Errorf("id = %v, want the artifact's own id %q (an entitlement id or a name would pass a presence-only check)", rawID, art.ID)
	}

	rawRev, ok := got["revision"]
	if !ok {
		t.Fatalf("the response carries no revision key at all (omitempty drops a 0, so this is what a projection reporting nothing looks like on the wire): %+v", got)
	}
	// encoding/json decodes every JSON number into float64 through any.
	if rev, isNum := rawRev.(float64); !isNum || int(rev) != wantRevision {
		t.Errorf("revision = %v (%T), want %d: the DTO must carry MAX(revision_num) for the artifact, not a constant and not a count of anything else",
			rawRev, rawRev, wantRevision)
	}
}
