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

// TestSyncArtifactsServesOrgVisibilityRules closes a silent black hole rather
// than adding a feature.
//
// Every other artifact type has two channels: org visibility ships through the
// Channel-1 marketplace plugin, role visibility through this endpoint. A `rule`
// (v1.14.0) has NO Channel 1 at all, by design, and
// marketplace.RenderArtifactsPlugin's type switch drops it in a `default` whose
// comment says "unknown type — skipped; Type is constrained at the DB + API
// layer". That comment stopped being true the day `rule` became a valid type.
//
// So before this: an admin creates a rule, leaves `visibility` at its default,
// which is `org` at both the API (admin_artifacts.go's "" defaults to org) and
// the column (migration 00004's DEFAULT 'org'), approves it, and it reaches
// NOBODY. Not one warning anywhere: not at create, not at approve, not at
// publish, not at sync. The default value is the broken one.
//
// The fix is the distribution this endpoint should always have done for the one
// type that has nowhere else to go, and the caller here deliberately holds NO
// roles: an org rule is for everyone who syncs, which is exactly what a user
// with no entitlements is.
func TestSyncArtifactsServesOrgVisibilityRules(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, _ := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("orgrule-%d", time.Now().UnixNano()))

	// The org rule: visibility left unset, so it takes the default that used to
	// mean "reaches nobody".
	orgRule, err := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "rule", Name: "house-style", Content: "Always measure before optimising.",
	})
	if err != nil {
		t.Fatalf("create org rule: %v", err)
	}
	approveArtifact(t, s, tn.ID, orgRule.ID)

	// An org SKILL must stay excluded: it has a Channel 1, and duplicating it
	// here would install it twice.
	orgSkill, _ := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "org-skill",
		Content: "---\nname: org-skill\ndescription: d\n---\nx",
	})
	approveArtifact(t, s, tn.ID, orgSkill.ID)

	// A role rule this caller is not entitled to must stay excluded: making org
	// rules universal must not make role rules universal too.
	roleRule, _ := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "rule", Name: "secret-rule", Content: "Only for the security role.",
		Visibility: "role",
	})
	approveArtifact(t, s, tn.ID, roleRule.ID)

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1"} // no roles at all
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncArtifacts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Artifacts []struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Artifacts) != 1 {
		t.Fatalf("want exactly the org rule, got %d artifacts: %+v", len(body.Artifacts), body.Artifacts)
	}
	got := body.Artifacts[0]
	if got.Name != "house-style" || got.Type != "rule" {
		t.Fatalf("got %s/%s, want rule/house-style", got.Type, got.Name)
	}
	if got.Content != "Always measure before optimising." {
		t.Fatalf("content = %q, want the rule body verbatim", got.Content)
	}
}

// TestSyncArtifactsWithholdsUnapprovedOrgRules is the governance half, and it
// exists because a mutant proved nothing else covered it: dropping the
// approved-snapshot guard from the new org-rule query left the whole package
// green while every draft rule in the tenant went out to every user.
//
// Phase 4's premise is that distribution serves the frozen last-approved
// content and nothing else. Making org rules universal widens the audience for
// exactly one type, so it also widens the blast radius of getting that premise
// wrong for it: a draft is written material nobody has signed off, and here it
// would land in every developer's AGENTS.md.
func TestSyncArtifactsWithholdsUnapprovedOrgRules(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, _ := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("orgruledraft-%d", time.Now().UnixNano()))

	// Never approved: no snapshot exists, so there is nothing legitimate to serve.
	if _, err := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "rule", Name: "draft-rule", Content: "NOT SIGNED OFF BY ANYONE",
	}); err != nil {
		t.Fatalf("create draft rule: %v", err)
	}
	// Approved, so the assertion below cannot pass merely because the endpoint
	// returns nothing at all.
	appr, _ := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "rule", Name: "approved-rule", Content: "Reviewed and approved.",
	})
	approveArtifact(t, s, tn.ID, appr.ID)

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/artifacts", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1"}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleSyncArtifacts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Artifacts []struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Artifacts) != 1 || body.Artifacts[0].Name != "approved-rule" {
		t.Fatalf("want only approved-rule, got %+v", body.Artifacts)
	}
	if strings.Contains(body.Artifacts[0].Content, "NOT SIGNED OFF") {
		t.Fatalf("the draft's content reached a sync client: %q", body.Artifacts[0].Content)
	}
}
