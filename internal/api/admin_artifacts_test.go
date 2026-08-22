package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// countingEnqueuer is a fake publish.Enqueuer that counts Enqueue calls.
type countingEnqueuer struct {
	n atomic.Int32
}

func (c *countingEnqueuer) Enqueue()   { c.n.Add(1) }
func (c *countingEnqueuer) count() int { return int(c.n.Load()) }

// newArtifactServer creates a test Server with a real store + a countingEnqueuer.
func newArtifactServer(t *testing.T) (*Server, *store.Store, store.Tenant, *countingEnqueuer) {
	t.Helper()
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, err := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("art-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	eq := &countingEnqueuer{}
	return New(s, nil, nil, nil, eq), s, tn, eq
}

// validSkillInput returns a valid skill artifact input body.
func validSkillInput() map[string]any {
	return map[string]any{
		"type":        "skill",
		"name":        "fmt-skill",
		"description": "formats code",
		"content":     "---\nname: fmt-skill\ndescription: formats code\n---\nrun gofmt .",
	}
}

// validSubagentInput returns a valid subagent artifact input body.
func validSubagentInput() map[string]any {
	return map[string]any{
		"type":        "subagent",
		"name":        "reviewer",
		"description": "reviews code",
		"content":     "---\nname: reviewer\ndescription: reviews code\n---\nyou are a code reviewer",
		"memoryScope": "project",
	}
}

func TestArtifactCreateSkill201(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, eq := newArtifactServer(t)

	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", validSkillInput(), tn))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name"] != "fmt-skill" || got["type"] != "skill" {
		t.Fatalf("unexpected body: %+v", got)
	}
	if got["id"] == "" {
		t.Fatal("id must be non-empty")
	}
	if eq.count() != 1 {
		t.Fatalf("Enqueue not called after create: count=%d", eq.count())
	}
}

func TestArtifactCreateSubagentWithMemoryScope201(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, eq := newArtifactServer(t)

	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", validSubagentInput(), tn))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["memoryScope"] != "project" {
		t.Fatalf("memoryScope not returned: %+v", got)
	}
	if eq.count() != 1 {
		t.Fatalf("Enqueue not called: count=%d", eq.count())
	}
}

func TestArtifactCreateMissingFrontmatterDescriptionIs400(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	in := map[string]any{
		"type":    "skill",
		"name":    "bad-skill",
		"content": "---\nname: bad-skill\n---\nbody",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing description = %d, want 400, body = %s", rec.Code, rec.Body)
	}
}

func TestArtifactCreateMemoryScopeOnSkillIs400(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	in := map[string]any{
		"type":        "skill",
		"name":        "my-skill",
		"content":     "---\nname: my-skill\ndescription: d\n---\nbody",
		"memoryScope": "project",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("memoryScope on skill = %d, want 400, body = %s", rec.Code, rec.Body)
	}
}

func TestArtifactCreateBadSlugNameIs400(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	in := map[string]any{
		"type":    "skill",
		"name":    "Bad_Name!",
		"content": "---\nname: Bad_Name!\ndescription: d\n---\nbody",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad slug = %d, want 400, body = %s", rec.Code, rec.Body)
	}
}

func TestArtifactCreateRulePlainContent201(t *testing.T) {
	// A rule's content is PLAIN markdown instruction text (no frontmatter);
	// it must NOT be run through the frontmatter validator.
	ctx := context.Background()
	srv, _, tn, eq := newArtifactServer(t)

	in := map[string]any{
		"type":        "rule",
		"name":        "plain-rule",
		"description": "d",
		"content":     "Never commit secrets.",
		"visibility":  "role",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))

	if rec.Code != http.StatusCreated {
		t.Fatalf("plain rule create = %d, want 201, body = %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["type"] != "rule" || got["name"] != "plain-rule" {
		t.Fatalf("unexpected body: %+v", got)
	}
	if got["content"] != "Never commit secrets." {
		t.Fatalf("content not stored verbatim: %+v", got)
	}
	if eq.count() != 1 {
		t.Fatalf("Enqueue not called after rule create: count=%d", eq.count())
	}
}

func TestArtifactCreateRuleEmptyContentIs400(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	in := map[string]any{
		"type":        "rule",
		"name":        "empty-rule",
		"description": "d",
		"content":     "   ",
		"visibility":  "role",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty rule content = %d, want 400, body = %s", rec.Code, rec.Body)
	}
}

// TestArtifactGetMalformedIDIs404 proves a non-UUID {id} maps to 404, not a
// 500 leaking a raw Postgres invalid_text_representation error.
func TestArtifactGetMalformedIDIs404(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodGet, "/v1/admin/artifacts/not-a-uuid", nil, tn)
	req.SetPathValue("id", "not-a-uuid")
	srv.handleGetArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id get = %d, want 404, body = %s", rec.Code, rec.Body)
	}
}

func TestArtifactList(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)
	_, _ = st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "listed-skill",
		Content: "---\nname: listed-skill\ndescription: d\n---\nx",
	})

	rec := httptest.NewRecorder()
	srv.handleListArtifacts(rec, adminReq(ctx, http.MethodGet, "/v1/admin/artifacts", nil, tn))

	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, body = %s", rec.Code, rec.Body)
	}
	var body struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Artifacts) != 1 || body.Artifacts[0]["name"] != "listed-skill" {
		t.Fatalf("list artifacts = %+v", body.Artifacts)
	}
}

func TestArtifactUpdate(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, eq := newArtifactServer(t)
	a, _ := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "upd-skill",
		Content: "---\nname: upd-skill\ndescription: original\n---\nx",
	})

	in := map[string]any{
		"type":    "skill",
		"name":    "upd-skill",
		"content": "---\nname: upd-skill\ndescription: updated desc\n---\nupdated body",
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/artifacts/"+a.ID, in, tn)
	req.SetPathValue("id", a.ID)
	req.Header.Set("If-Match", etag(a.RowVersion)) // a freshly-created row: row_version=1
	srv.handleUpdateArtifact(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body = %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["content"] != in["content"] {
		t.Fatalf("content not updated: %+v", got)
	}
	// rowVersion is asserted RELATIVE to the If-Match precondition, not
	// against an absolute 2, and approvalState is not asserted here at all.
	// Both were absolute before, and both are edition-specific through this
	// handler: a generated Community tree auto-approves inside the same
	// transaction, so the row takes a second UPDATE and lands on rowVersion
	// 3 with approvalState "approved". This is an ordinary _test.go that
	// communitygen copies verbatim, so it was failing there on a count of
	// internal writes it never cared about. What it does care about, and
	// what is true in both editions, is that the artifact_bump_row_version
	// trigger fired and the ETag describes the row the body just returned.
	// The Enterprise "an edit returns to draft" statement lives in
	// admin_artifacts.ee_test.go and, with a live prior approval, in
	// autoapprove.ee_test.go's
	// TestHandleUpdateArtifactDoesNotReAutoApproveByDefault.
	rv, ok := got["rowVersion"].(float64)
	if !ok {
		t.Fatalf("rowVersion missing or not a number: %+v", got)
	}
	if int64(rv) <= a.RowVersion {
		t.Fatalf("rowVersion = %v, want greater than the If-Match precondition %d, the update trigger did not fire",
			rv, a.RowVersion)
	}
	if e := rec.Header().Get("ETag"); e != etag(int64(rv)) {
		t.Fatalf("ETag = %q, want %q, it must describe the rowVersion the body returned", e, etag(int64(rv)))
	}
	if eq.count() != 1 {
		t.Fatalf("Enqueue not called on update: count=%d", eq.count())
	}
}

func TestArtifactUpdateMemorySeedFullReplaceClears(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)
	a, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "seeded-rev",
		Description: "reviews code",
		Content:     "---\nname: seeded-rev\ndescription: reviews code\n---\nyou are a code reviewer",
		MemoryScope: "user",
	})
	if err != nil {
		t.Fatalf("create subagent: %v", err)
	}

	// PUT with a memorySeed → echoed DTO carries it.
	withSeed := map[string]any{
		"type":        "subagent",
		"name":        "seeded-rev",
		"description": "reviews code",
		"content":     "---\nname: seeded-rev\ndescription: reviews code\n---\nyou are a code reviewer",
		"memoryScope": "user",
		"memorySeed":  "seed v1",
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/artifacts/"+a.ID, withSeed, tn)
	req.SetPathValue("id", a.ID)
	req.Header.Set("If-Match", etag(a.RowVersion)) // a freshly-created row: row_version=1
	srv.handleUpdateArtifact(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update with seed = %d, body = %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["memorySeed"] != "seed v1" {
		t.Fatalf("memorySeed not echoed back: %+v", got)
	}
	rv, ok := got["rowVersion"].(float64)
	if !ok {
		t.Fatalf("rowVersion missing or not a number: %+v", got)
	}

	// PUT again WITHOUT memorySeed (full replace) → echoed DTO is cleared.
	withoutSeed := map[string]any{
		"type":        "subagent",
		"name":        "seeded-rev",
		"description": "reviews code",
		"content":     "---\nname: seeded-rev\ndescription: reviews code\n---\nyou are a code reviewer",
		"memoryScope": "user",
	}
	rec2 := httptest.NewRecorder()
	req2 := adminReq(ctx, http.MethodPut, "/v1/admin/artifacts/"+a.ID, withoutSeed, tn)
	req2.SetPathValue("id", a.ID)
	req2.Header.Set("If-Match", etag(int64(rv))) // the version the first PUT just bumped to
	srv.handleUpdateArtifact(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("update without seed = %d, body = %s", rec2.Code, rec2.Body)
	}
	var got2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &got2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got2["memorySeed"] != "" {
		t.Fatalf("full-replace update must clear memorySeed, got: %+v", got2)
	}
}

func TestArtifactDeleteUnknownIs404(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodDelete, "/v1/admin/artifacts/00000000-0000-0000-0000-000000000000", nil, tn)
	req.SetPathValue("id", "00000000-0000-0000-0000-000000000000")
	srv.handleDeleteArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown = %d, want 404", rec.Code)
	}
}

// TestArtifactDeleteMalformedIDIs404 proves a non-UUID {id} maps to 404 on
// delete too (DeleteArtifact is not preceded by a getter in the handler).
func TestArtifactDeleteMalformedIDIs404(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodDelete, "/v1/admin/artifacts/not-a-uuid", nil, tn)
	req.SetPathValue("id", "not-a-uuid")
	srv.handleDeleteArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed id delete = %d, want 404, body = %s", rec.Code, rec.Body)
	}
}

func TestArtifactDeleteAuditedAnd204(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, eq := newArtifactServer(t)
	a, _ := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "del-skill",
		Content: "---\nname: del-skill\ndescription: d\n---\nbody",
	})

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodDelete, "/v1/admin/artifacts/"+a.ID, nil, tn)
	req.SetPathValue("id", a.ID)
	srv.handleDeleteArtifact(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", rec.Code)
	}
	if _, err := st.GetArtifact(ctx, tn.ID, a.ID); err == nil {
		t.Fatal("artifact not deleted")
	}
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if len(evs) != 1 || evs[0].Action != "artifact.delete" || evs[0].Decision != "allow" {
		t.Fatalf("expected one artifact.delete audit, got %+v", evs)
	}
	if eq.count() != 1 {
		t.Fatalf("Enqueue not called on delete: count=%d", eq.count())
	}
}

func TestArtifactCreateAuditRecorded(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)

	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", validSkillInput(), tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}

	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if len(evs) != 1 || evs[0].Action != "artifact.create" || evs[0].Decision != "allow" {
		t.Fatalf("expected artifact.create audit, got %+v", evs)
	}
}

func TestArtifactRBACNonAdminIs403(t *testing.T) {
	// Mirror TestAdminRouteRequiresAdminRole: RequireRole denies a non-admin with 403.
	h := authz.RequireRole("orbeat-admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/artifacts", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Subject: "u", Roles: []string{"orbeat-user"}}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", rec.Code)
	}
}

func TestArtifactDuplicateNameIs409(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	body := map[string]any{
		"type":        "skill",
		"name":        "dup",
		"description": "d",
		"content":     "---\nname: dup\ndescription: d\n---\nx",
	}
	// first create → 201
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", body, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d, want 201, body = %s", rec.Code, rec.Body)
	}
	// second create with the same (type, name) → 409
	rec2 := httptest.NewRecorder()
	srv.handleCreateArtifact(rec2, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", body, tn))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409, body = %s", rec2.Code, rec2.Body)
	}
}

func TestArtifactUpdateMemoryScopeOnSkillIs400(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)

	// create a valid subagent with memoryScope → 201, capture id
	a, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID:    tn.ID,
		Type:        "subagent",
		Name:        "rev",
		Description: "reviews code",
		Content:     "---\nname: rev\ndescription: reviews code\n---\nyou are a code reviewer",
		MemoryScope: "project",
	})
	if err != nil {
		t.Fatalf("create subagent: %v", err)
	}

	// PUT an update that sets type:skill while keeping a non-empty memoryScope → 400
	in := map[string]any{
		"type":        "skill",
		"name":        "rev",
		"description": "reviews code",
		"content":     "---\nname: rev\ndescription: reviews code\n---\nyou are a code reviewer",
		"memoryScope": "project",
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/artifacts/"+a.ID, in, tn)
	req.SetPathValue("id", a.ID)
	req.Header.Set("If-Match", etag(a.RowVersion)) // well-formed; validateArtifact rejects before this is ever compared
	srv.handleUpdateArtifact(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("skill with memoryScope on update = %d, want 400, body = %s", rec.Code, rec.Body)
	}
}

func TestArtifactCreateWithRoleVisibility(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)
	in := validSkillInput()
	in["visibility"] = "role"
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["visibility"] != "role" {
		t.Fatalf("visibility=%v, want role", got["visibility"])
	}
}

func TestArtifactCreateDefaultsToOrgVisibility(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", validSkillInput(), tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["visibility"] != "org" {
		t.Fatalf("default visibility=%v, want org", got["visibility"])
	}
}

func TestArtifactCreateInvalidVisibilityIs400(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)
	in := validSkillInput()
	in["visibility"] = "secret"
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid visibility=%d, want 400, body=%s", rec.Code, rec.Body)
	}
}

// TestArtifactCreateOversizedContentIs400 proves content beyond the 64KiB hard
// cap is rejected outright (audit B3, BREAKING: previously accepted, gated
// only by the govern.Scanner's warn-level finding at submit time). The cap is
// govern.MaxContentBytes (64 * 1024), referenced directly by validateArtifact.
func TestArtifactCreateOversizedContentIs400(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)
	in := validSkillInput()
	in["content"] = "---\nname: fmt-skill\ndescription: formats code\n---\n" + strings.Repeat("a", 70_000)
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized content = %d, want 400, body = %s", rec.Code, rec.Body)
	}
}

// TestArtifactCreateOversizedSeedIs400 proves memorySeed beyond the 16KiB hard
// cap is rejected outright (audit B3, BREAKING). The cap is govern.MaxSeedBytes
// (16 * 1024), referenced directly by validateArtifact.
func TestArtifactCreateOversizedSeedIs400(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)
	in := validSubagentInput()
	in["memorySeed"] = strings.Repeat("a", 20_000)
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized seed = %d, want 400, body = %s", rec.Code, rec.Body)
	}
}

func TestMarketplacePublishEnqueuesAnd202(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, eq := newArtifactServer(t)

	rec := httptest.NewRecorder()
	srv.handleMarketplacePublish(rec, adminReq(ctx, http.MethodPost, "/v1/admin/marketplace/publish", nil, tn))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("publish = %d, want 202, body = %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "queued" {
		t.Fatalf("unexpected body: %+v", got)
	}
	if eq.count() != 1 {
		t.Fatalf("Enqueue not called on publish: count=%d", eq.count())
	}
}

// TestMarketplacePublishIsAudited proves the admin-triggered publish appends a
// best-effort marketplace.publish/allow audit event (audit B6: it was the only
// admin mutation-adjacent action with no audit trail).
func TestMarketplacePublishIsAudited(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, _ := newArtifactServer(t)

	rec := httptest.NewRecorder()
	srv.handleMarketplacePublish(rec, adminReq(ctx, http.MethodPost, "/v1/admin/marketplace/publish", nil, tn))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("publish = %d, body %s", rec.Code, rec.Body)
	}
	evs, _ := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	var saw bool
	for _, e := range evs {
		if e.Action == "marketplace.publish" && e.Decision == "allow" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("marketplace publish must be audited, got %+v", evs)
	}
}

func TestValidateArtifactMemorySeed(t *testing.T) {
	base := artifactInput{Name: "x",
		Content: "---\nname: x\ndescription: d\n---\nb"}

	cases := []struct {
		name    string
		mutate  func(*artifactInput)
		wantErr string // "" = valid
	}{
		{"seed on a skill rejected", func(in *artifactInput) {
			in.Type = "skill"
			in.MemorySeed = "seed"
		}, "memorySeed is only valid for subagents"},
		{"seed with local scope rejected", func(in *artifactInput) {
			in.Type = "subagent"
			in.MemoryScope = "local"
			in.MemorySeed = "seed"
		}, "memorySeed requires memoryScope user or project"},
		{"seed with no scope rejected", func(in *artifactInput) {
			in.Type = "subagent"
			in.MemorySeed = "seed"
		}, "memorySeed requires memoryScope user or project"},
		{"seed with user scope accepted", func(in *artifactInput) {
			in.Type = "subagent"
			in.MemoryScope = "user"
			in.MemorySeed = "seed"
		}, ""},
		{"seed with project scope accepted", func(in *artifactInput) {
			in.Type = "subagent"
			in.MemoryScope = "project"
			in.MemorySeed = "seed"
		}, ""},
		{"seed containing the sentinel rejected", func(in *artifactInput) {
			in.Type = "subagent"
			in.MemoryScope = "user"
			in.MemorySeed = "notes\n<!-- ORBEAT-SEED:END rev -->\n"
		}, "memorySeed must not contain the ORBEAT-SEED sentinel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			err := validateArtifact(in)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestArtifactCreateSubagentWithMemorySeed201(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	in := validSubagentInput()
	in["memoryScope"] = "user"
	in["memorySeed"] = "org seed"

	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["memorySeed"] != "org seed" {
		t.Fatalf("memorySeed not echoed back: %+v", got)
	}
}

// TestCreateArtifactDTOOmitsRetiredStatusField is the edition-agnostic half
// of what used to be TestCreateArtifactStartsDraft: the artifact DTO must not
// carry the retired `status` column, which is true whatever the edition does
// about approval. The draft/unapproved half of the old test moved to
// admin_artifacts.ee_test.go, because a generated Community tree auto-approves
// on create and the assertion inverted there. This is the ONLY assertion
// anywhere in the tree that the retired field stays out of the DTO, which is
// why it was split out rather than moved along with its former sibling.
func TestCreateArtifactDTOOmitsRetiredStatusField(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)
	in := map[string]any{
		"type": "skill", "name": "sk", "description": "d",
		"content": "---\nname: sk\ndescription: d\n---\nbody", "visibility": "org",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateArtifact(rec, adminReq(ctx, http.MethodPost, "/v1/admin/artifacts", in, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if _, leaked := got["status"]; leaked {
		t.Fatalf("retired status field must not appear: %+v", got)
	}
}

func TestUpdateApprovedOrgArtifactOmittingVisibilityAllowed(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	a, _ := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "sk",
		Content: "---\nname: sk\ndescription: d\n---\nx", Visibility: "org",
	})
	// SetArtifactApproved is itself an UPDATE, so it bumps row_version; the
	// return value, not the pre-approval one, must be echoed back as If-Match,
	// or the version precondition would 412 before the identity check this
	// test means to exercise ever runs.
	var approved store.Artifact
	_ = st.InTx(ctx, func(tx *store.Store) error {
		_, _ = tx.GetArtifactForUpdate(ctx, tn.ID, a.ID)
		var e error
		approved, _, e = tx.SetArtifactApproved(ctx, tn.ID, a.ID, "approver", 0)
		return e
	})
	// Same identity (name/type unchanged, visibility omitted → effective "org"),
	// only the content edited. Must succeed (200), not be locked (400).
	in := map[string]any{
		"type": "skill", "name": "sk", "description": "d2",
		"content": "---\nname: sk\ndescription: d2\n---\nEDITED",
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/artifacts/"+a.ID, in, tn)
	req.SetPathValue("id", a.ID)
	req.Header.Set("If-Match", etag(approved.RowVersion))
	srv.handleUpdateArtifact(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("content-only update on approved org artifact = %d, want 200, body %s", rec.Code, rec.Body)
	}
}

func TestMarketplaceStatusReturnsJSON(t *testing.T) {
	ctx := context.Background()
	srv, _, tn, _ := newArtifactServer(t)

	rec := httptest.NewRecorder()
	srv.handleMarketplaceStatus(rec, adminReq(ctx, http.MethodGet, "/v1/admin/marketplace/status", nil, tn))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Fields must be present (even if null/empty for a fresh tenant).
	for _, k := range []string{"lastAttemptAt", "lastSuccessAt", "lastCommit", "lastError"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing key %q in publish status: %+v", k, got)
		}
	}
}

// TestAdminUpdateArtifactRequiresIfMatch drives the REAL router (Task 7 of
// the optimistic-concurrency plan, spec §5/§6.1/§9), real RS256-token auth,
// real admin-role gate, real mux dispatch (see mwOrderTestIdP,
// middleware_order_test.go), mirroring TestAdminUpdateServerRequiresIfMatch
// (admin_test.go, Task 6) exactly: the precondition is enforced inside
// handleUpdateArtifact, but the CORS/role/resolver wiring sits ABOVE it, so
// only the router proves the full chain, not a direct handler call like
// every other test in this file.
//
// Subtests run in declared order and share one seeded row: the assertions
// depend on that (the 412 case must run while row_version is still 1; the
// 200 case then bumps it to 2, and the malformed-id case runs last since its
// If-Match value no longer matters). The cross-tenant case (spec §5.2) is a
// separate test below, mirroring TestAdminGetServerOtherTenantIs404's
// established direct-handler pattern for tenant isolation in this package,
// tenant scoping is enforced INSIDE the handler (GetArtifactForUpdate), not
// by anything the router adds, so a direct call is equally decisive evidence
// there without the overhead of a second full router+resolver stack.
func TestAdminUpdateArtifactRequiresIfMatch(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tenantName := fmt.Sprintf("ifmatch-artifacts-%d", time.Now().UnixNano())
	idp := newMWOrderTestIdP(t)
	v, err := auth.NewValidator(ctx, auth.Config{Issuer: idp.srv.URL, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	srv := New(st, authz.NewResolver(st, tenantName), v, nil, nil)
	tok := idp.token(t, "kc-ifmatch-artifact-admin", []string{"orbeat-admin"})

	tn, err := st.GetOrCreateTenantByName(ctx, tenantName)
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	orig, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "ifmatch-artifact",
		Content: "---\nname: ifmatch-artifact\ndescription: d\n---\noriginal body",
	})
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if orig.RowVersion != 1 {
		t.Fatalf("seed RowVersion = %d, want 1", orig.RowVersion)
	}

	put := func(id string, ifMatch *string, body string) *httptest.ResponseRecorder {
		in, _ := json.Marshal(map[string]any{
			"type": "skill", "name": "ifmatch-artifact", "description": "d",
			"content": "---\nname: ifmatch-artifact\ndescription: d\n---\n" + body,
		})
		req := httptest.NewRequest(http.MethodPut, "/v1/admin/artifacts/"+id, bytes.NewReader(in))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		if ifMatch != nil {
			req.Header.Set("If-Match", *ifMatch)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	strp := func(s string) *string { return &s }

	t.Run("no If-Match is 428", func(t *testing.T) {
		rec := put(orig.ID, nil, "attempt-no-header")
		if rec.Code != http.StatusPreconditionRequired {
			t.Fatalf("status = %d, want 428, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("If-Match: * is 428", func(t *testing.T) {
		rec := put(orig.ID, strp("*"), "attempt-wildcard")
		if rec.Code != http.StatusPreconditionRequired {
			t.Fatalf("status = %d, want 428, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("unquoted If-Match is 400", func(t *testing.T) {
		rec := put(orig.ID, strp("7"), "attempt-unquoted")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
		}
	})

	// The decisive case: a stale If-Match must 412 AND must not touch the row.
	// A 412 that still writes is worse than no guard at all.
	t.Run("stale If-Match is 412 and does not mutate", func(t *testing.T) {
		rec := put(orig.ID, strp(`"999"`), "attempt-stale")
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("status = %d, want 412, body = %s", rec.Code, rec.Body)
		}
		a, err := st.GetArtifact(ctx, tn.ID, orig.ID)
		if err != nil {
			t.Fatalf("reread: %v", err)
		}
		if a.Content != orig.Content {
			t.Fatalf("412 must not mutate the row, content = %q", a.Content)
		}
		if a.RowVersion != 1 {
			t.Fatalf("412 must not bump row_version, got %d", a.RowVersion)
		}
		// v1.17.0 finding B1: a deny decision must leave a durable trace, not
		// just a status code. A 412 IS a rejected mutation.
		evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
		if err != nil {
			t.Fatalf("audit list: %v", err)
		}
		found := false
		for _, ev := range evs {
			if ev.Action == "artifact.update" && ev.Decision == "deny" && ev.Target == orig.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected a deny-audited artifact.update for the stale If-Match, got %+v", evs)
		}
	})

	t.Run("current If-Match is 200 with bumped rowVersion and matching ETag", func(t *testing.T) {
		rec := put(orig.ID, strp(`"1"`), "attempt-current")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body)
		}
		var got struct {
			RowVersion int64  `json:"rowVersion"`
			Content    string `json:"content"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// Asserted RELATIVE to the "1" precondition this PUT carried, not
		// against an absolute 2. If-Match semantics are identical in both
		// editions, but this is an ordinary _test.go that communitygen copies
		// verbatim into a tree where auto-approve performs a second UPDATE
		// inside the same transaction and the row lands on 3, so the
		// absolute number was measuring internal writes, which this test
		// never cared about, and it failed the generated tree for a reason
		// unrelated to preconditions. The contract is that a satisfied
		// precondition advances the version and that the ETag describes the
		// version the body returned.
		if got.RowVersion <= 1 {
			t.Fatalf("rowVersion = %d, want greater than the satisfied If-Match precondition 1", got.RowVersion)
		}
		if e := rec.Header().Get("ETag"); e != etag(got.RowVersion) {
			t.Fatalf("ETag = %q, want %q, it must describe the rowVersion the body returned", e, etag(got.RowVersion))
		}
		if !strings.Contains(got.Content, "attempt-current") {
			t.Fatalf("content not updated: %+v", got)
		}
	})

	// The v1.16.0 class: a malformed path id must still 404, not 500, even
	// with a well-formed If-Match. GetArtifactForUpdate's own idCastNotFound
	// mapping must carry this alone, mirroring the equivalent server-path test.
	t.Run("malformed path id with valid If-Match is 404", func(t *testing.T) {
		rec := put("not-a-uuid", strp(`"1"`), "attempt-malformed-id")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body)
		}
	})
}

// TestArtifactUpdateCrossTenantIdIs404NotVersionMismatch proves spec §5.2's
// deliberate deviation from RFC 9110 holds for artifacts too: a well-formed
// If-Match against a row that belongs to ANOTHER tenant must stay 404, not
// 412, a 412 would leak "this id exists" across the tenant boundary, and
// would be flatly wrong regardless (the requester never saw ANY version of a
// row it cannot see). GetArtifactForUpdate is tenant-scoped in SQL, so the
// row is invisible before the version comparison in Go is ever reached,
// mirrors TestAdminGetServerOtherTenantIs404's established pattern for this
// package.
func TestArtifactUpdateCrossTenantIdIs404NotVersionMismatch(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	other, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("other-artifact-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	foreign, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: other.ID, Type: "skill", Name: "foreign-skill",
		Content: "---\nname: foreign-skill\ndescription: d\n---\nforeign body",
	})
	if err != nil {
		t.Fatalf("create foreign artifact: %v", err)
	}

	in := map[string]any{
		"type": "skill", "name": "foreign-skill", "description": "d",
		"content": "---\nname: foreign-skill\ndescription: d\n---\ncross-tenant-attempt",
	}
	rec := httptest.NewRecorder()
	// Authenticated as an admin of OUR tenant (tn), targeting an id that only
	// exists under `other`. The If-Match value is deliberately well-formed
	// AND equal to the foreign row's actual row_version (1), if the handler
	// mistakenly compared versions before (or instead of) checking tenant
	// scope, this would let a 412 slip through undetected.
	req := adminReq(ctx, http.MethodPut, "/v1/admin/artifacts/"+foreign.ID, in, tn)
	req.SetPathValue("id", foreign.ID)
	req.Header.Set("If-Match", etag(foreign.RowVersion))
	srv.handleUpdateArtifact(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant update = %d, want 404, body = %s", rec.Code, rec.Body)
	}
	reread, err := st.GetArtifact(ctx, other.ID, foreign.ID)
	if err != nil {
		t.Fatalf("reread foreign artifact: %v", err)
	}
	if strings.Contains(reread.Content, "cross-tenant-attempt") {
		t.Fatalf("cross-tenant attempt must not mutate the row, content = %q", reread.Content)
	}
}
