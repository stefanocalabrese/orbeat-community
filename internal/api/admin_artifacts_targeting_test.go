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

// TestValidateTargetTags covers the shape the DATABASE CANNOT enforce. A CHECK
// constraint may not contain a subquery, so `unnest`-ing the array to regex each
// element is not expressible in SQL; migration 00024 carries the arity and the
// rule-only restriction, and everything else is only ever checked here.
func TestValidateTargetTags(t *testing.T) {
	rule := func(tags ...string) artifactInput {
		return artifactInput{Type: "rule", Name: "r", Content: "body", TargetTags: tags}
	}
	many := make([]string, 17)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}

	for _, tc := range []struct {
		name string
		in   artifactInput
		ok   bool
	}{
		{"no tags", rule(), true},
		{"one tag", rule("go"), true},
		{"sixteen tags", rule(many[:16]...), true},
		{"seventeen tags", rule(many...), false},
		{"uppercase", rule("Go"), false},
		{"space", rule("go lang"), false},
		{"underscore", rule("go_lang"), false},
		{"empty string", rule(""), false},
		{"duplicate", rule("go", "go"), false},
		{"on a skill", artifactInput{Type: "skill", Name: "s", Content: "x", TargetTags: []string{"go"}}, false},
		{"on a subagent", artifactInput{Type: "subagent", Name: "s", Content: "x", TargetTags: []string{"go"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTargetTags(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("want rejected, got nil")
			}
		})
	}
}

// TestSyncServesApprovedTargetTagsOnly is the governance half, and it is the
// same argument that put approved_visibility in migration 00016: re-targeting a
// rule changes WHO RECEIVES IT, so it waits for an approval. Without the
// snapshot an admin could widen a reviewed rule to every project in the org
// with no second pair of eyes, which is exactly the review that Phase 4 exists
// to require.
func TestSyncServesApprovedTargetTagsOnly(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, _ := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("tags-%d", time.Now().UnixNano()))

	rule, err := s.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "rule", Name: "scoped", Content: "SCOPED-BODY",
		TargetTags: []string{"go"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	approveArtifact(t, s, tn.ID, rule.ID)

	// Re-read after approval: approving bumps row_version, so the create-time
	// value is stale and the update below would fail its optimistic check for a
	// reason that has nothing to do with what this test is about.
	fresh, err := s.GetArtifact(ctx, tn.ID, rule.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	// Re-target the working copy WITHOUT approving it.
	fresh.TargetTags = []string{"go", "rust", "python"}
	if _, err := s.UpdateArtifact(ctx, fresh, fresh.RowVersion); err != nil {
		t.Fatalf("update: %v", err)
	}

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
			Name       string   `json:"name"`
			TargetTags []string `json:"targetTags"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Artifacts) != 1 {
		t.Fatalf("want the one org rule, got %+v", body.Artifacts)
	}
	got := strings.Join(body.Artifacts[0].TargetTags, ",")
	if got != "go" {
		t.Fatalf("sync served targeting %q, want the APPROVED %q — an unapproved widening reached clients", got, "go")
	}
}

// TestValidateRuleScope covers the two values migration 00025 allows plus the
// one combination that is meaningless rather than merely unusual: a GLOBAL rule
// carrying target tags. Tags select projects; a global rule is written into
// none, so storing the pair would record a constraint that can never be
// consulted, which reads to an admin as targeting that is being applied.
func TestValidateRuleScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   artifactInput
		ok   bool
	}{
		{"absent", artifactInput{Type: "rule", Name: "r", Content: "b"}, true},
		{"project", artifactInput{Type: "rule", Name: "r", Content: "b", RuleScope: "project"}, true},
		{"global", artifactInput{Type: "rule", Name: "r", Content: "b", RuleScope: "global"}, true},
		{"nonsense value", artifactInput{Type: "rule", Name: "r", Content: "b", RuleScope: "user"}, false},
		{"on a skill", artifactInput{Type: "skill", Name: "s", Content: "b", RuleScope: "global"}, false},
		{"global with tags", artifactInput{Type: "rule", Name: "r", Content: "b", RuleScope: "global", TargetTags: []string{"go"}}, false},
		{"project with tags is fine", artifactInput{Type: "rule", Name: "r", Content: "b", RuleScope: "project", TargetTags: []string{"go"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRuleScope(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("want rejected, got nil")
			}
		})
	}
}
