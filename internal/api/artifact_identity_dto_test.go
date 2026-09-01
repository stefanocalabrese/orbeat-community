package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// listArtifactRows drives GET /v1/admin/artifacts with the given query string
// and returns its rows as raw maps, keyed by artifact id.
//
// Raw maps rather than []artifactDTO deliberately. Decoding into the struct
// would turn an absent approvedName and an empty one into the same "", and the
// difference between them is the whole point of the omitempty on those fields:
// absent means nothing is distributed, empty would mean distributed under an
// empty name. A typed decode cannot fail for that.
func listArtifactRows(t *testing.T, srv *Server, tn store.Tenant, query string) map[string]map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleListArtifacts(rec, adminReq(context.Background(), http.MethodGet, "/v1/admin/artifacts"+query, nil, tn))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/admin/artifacts%s = %d, body %s", query, rec.Code, rec.Body)
	}
	var body struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v, raw=%s", err, rec.Body)
	}
	out := make(map[string]map[string]any, len(body.Artifacts))
	for _, row := range body.Artifacts {
		id, _ := row["id"].(string)
		out[id] = row
	}
	return out
}

// getArtifactRow drives GET /v1/admin/artifacts/{id} and returns the raw body.
func getArtifactRow(t *testing.T, srv *Server, tn store.Tenant, id string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := adminReq(context.Background(), http.MethodGet, "/v1/admin/artifacts/"+id, nil, tn)
	req.SetPathValue("id", id)
	srv.handleGetArtifact(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/admin/artifacts/%s = %d, body %s", id, rec.Code, rec.Body)
	}
	var row map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode artifact: %v, raw=%s", err, rec.Body)
	}
	return row
}

// assertDistributedIdentity checks the three approved_* identity fields by
// VALUE, alongside the three live ones they are supposed to differ from.
//
// Both triples, always. Asserting only the approved side passes on a response
// that reports the approved identity in the live fields too, which is the
// pre-00016 behaviour this whole slice replaced; asserting only the live side
// passes on a response that carries no approved identity at all.
func assertDistributedIdentity(t *testing.T, where string, row map[string]any, liveType, liveName, liveVis, appType, appName, appVis string) {
	t.Helper()
	for _, f := range []struct{ key, want string }{
		{"type", liveType}, {"name", liveName}, {"visibility", liveVis},
		{"approvedType", appType}, {"approvedName", appName}, {"approvedVisibility", appVis},
	} {
		if got, _ := row[f.key].(string); got != f.want {
			t.Errorf("%s: %s = %v, want %q", where, f.key, row[f.key], f.want)
		}
	}
}

// TestArtifactDTOCarriesTheDistributedIdentity is the DTO round trip for
// migration 00016's three approved_* identity columns, and the LIST row is the
// half that carries the risk.
//
// GET /v1/admin/artifacts is served by a slim projection that replaces the four
// heavy payload columns with placeholders (artifactSlimCols), and the identity
// columns ride in it as real values while approvedContent next to them does
// not. That asymmetry is deliberate (they are slug-sized, and the row-level
// pending marker needs them without ?include=content) and it is exactly the
// kind of thing a later audit of that projection "tidies away", so the list
// assertion is separate from the by-id one: dropping the three from
// artifactSlimCols reddens the list and leaves the by-id read green, which is
// the whole reason to test the list row at all.
//
// The artifact is left with a PENDING identity edit, so all six fields carry
// different values. Without that, approvedName == name and the assertion would
// pass on a handler that filled the approved fields from the live row.
func TestArtifactDTOCarriesTheDistributedIdentity(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)
	// Pinned rather than left at New's edition-specific default: under
	// auto-approve the edit below promotes itself in the same transaction and
	// the live and approved identities never differ, so there would be nothing
	// here to tell apart. Community's own behaviour is autoapprove_test.go's.
	srv.autoApprove = false

	art, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "dto-before", Description: "d",
		Content: "---\nname: dto-before\ndescription: d\n---\nBODY-SENTINEL", Visibility: "org",
	})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	approveArtifact(t, st, tn.ID, art.ID)
	editArtifactViaAPI(t, srv, st, tn, art.ID, func(a *store.Artifact) {
		a.Type, a.Name, a.Visibility = "subagent", "dto-after", "role"
		a.Content = "---\nname: dto-after\ndescription: d\n---\nBODY-SENTINEL"
	})

	// A second artifact that was never approved: its three approved_* fields
	// must be ABSENT, not empty strings. A client cannot render "distributing
	// as X" off a field that says "" for both "nothing is distributed" and
	// "distributed under an empty name", and the
	// artifact_approved_identity_complete CHECK is what makes absent-together
	// an invariant rather than a habit.
	draft, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "dto-draft", Description: "d",
		Content: "---\nname: dto-draft\ndescription: d\n---\nbody", Visibility: "org",
	})
	if err != nil {
		t.Fatalf("create draft artifact: %v", err)
	}

	// THE GATE, on the list row, with NO ?include=content.
	rows := listArtifactRows(t, srv, tn, "")
	row, ok := rows[art.ID]
	if !ok {
		t.Fatalf("artifact %s missing from the list, got %d rows", art.ID, len(rows))
	}
	assertDistributedIdentity(t, "list row", row, "subagent", "dto-after", "role", "skill", "dto-before", "org")
	// The projection is still slim: a "fix" that carried the identity by
	// dropping the slimming would satisfy every assertion above while putting
	// ~160 KiB back on every listed row.
	if got, _ := row["content"].(string); got != "" {
		t.Errorf("list row content = %q, want empty without ?include=content", got)
	}
	if _, present := row["approvedContent"]; present {
		t.Errorf("list row carries approvedContent = %v, want it omitted without ?include=content", row["approvedContent"])
	}

	draftRow, ok := rows[draft.ID]
	if !ok {
		t.Fatalf("draft artifact %s missing from the list", draft.ID)
	}
	for _, k := range []string{"approvedType", "approvedName", "approvedVisibility"} {
		if v, present := draftRow[k]; present {
			t.Errorf("unapproved artifact carries %s = %v, want the key absent: nothing is distributed", k, v)
		}
	}

	// The by-id read, which the slim projection does not serve, must agree.
	byID := getArtifactRow(t, srv, tn, art.ID)
	assertDistributedIdentity(t, "by-id", byID, "subagent", "dto-after", "role", "skill", "dto-before", "org")
	if got, _ := byID["approvedContent"].(string); got == "" {
		t.Error("by-id approvedContent is empty: the by-id read is the full projection")
	}
}
