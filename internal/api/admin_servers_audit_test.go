package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// assertServerAuditMetadata checks one audit row against the four fields
// serverWriteAuditMetadata must carry. Each is a separate Errorf on purpose:
// deleting any ONE of them from the helper has to fail this test on its own
// name, or the gate degrades into "some metadata exists".
func assertServerAuditMetadata(t *testing.T, ev store.AuditEvent, name, endpoint, secretRef, tlsCARef string) {
	t.Helper()
	for _, c := range []struct{ key, want string }{
		{"name", name},
		{"endpointOrCommand", endpoint},
		{"secretRef", secretRef},
		{"tlsCaRef", tlsCARef},
	} {
		got, present := ev.Metadata[c.key]
		if !present {
			t.Errorf("%s audit metadata has no %q; the write is not reviewable without it (metadata %v)",
				ev.Action, c.key, ev.Metadata)
			continue
		}
		if got != c.want {
			t.Errorf("%s audit metadata[%q] = %v, want %q", ev.Action, c.key, got, c.want)
		}
	}
}

// TestServerWriteAuditsEndpointAndRefs is audit A4's aggravating factor: both
// write paths recorded only {"name": ...}, so an admin adding a legitimate MCP
// server and an admin pointing one at their own collector with a credential ref
// of their choosing produced identical audit rows.
//
// The values asserted are the ones that make the two distinguishable, which is
// why the fixture uses the attack's own endpoint rather than a neutral one.
func TestServerWriteAuditsEndpointAndRefs(t *testing.T) {
	t.Setenv("ORBEAT_SECRET_ENV_ALLOW", "")
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", map[string]any{
		"name": "a4-audit", "transport": "http", "endpointOrCommand": exfilEndpoint,
		"secretRef": "env:ORBEAT_UPSTREAM_TOKEN", "tlsCaRef": "env:ORBEAT_UPSTREAM_CA",
		"status": "active",
	}, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (body %s)", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := created["id"].(string)
	rowVersion, _ := created["rowVersion"].(float64)

	evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	if len(evs) != 1 || evs[0].Action != "server.create" {
		t.Fatalf("want one server.create audit, got %+v", evs)
	}
	assertServerAuditMetadata(t, evs[0], "a4-audit", exfilEndpoint,
		"env:ORBEAT_UPSTREAM_TOKEN", "env:ORBEAT_UPSTREAM_CA")

	// Update: the row must record what the server BECAME, and clearing a ref
	// has to be visible as "" rather than as an absent key, since "this server
	// no longer carries a credential" is itself the reviewable fact.
	rec = httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+id, map[string]any{
		"name": "a4-audit", "transport": "http", "endpointOrCommand": "https://ok.example/mcp",
		"secretRef": "", "tlsCaRef": "", "status": "active",
	}, tn)
	req.SetPathValue("id", id)
	req.Header.Set("If-Match", etag(int64(rowVersion)))
	srv.handleUpdateServer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status = %d (body %s)", rec.Code, rec.Body)
	}
	evs, err = st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	if len(evs) != 2 || evs[0].Action != "server.update" {
		t.Fatalf("want a server.update audit on top, got %+v", evs)
	}
	assertServerAuditMetadata(t, evs[0], "a4-audit", "https://ok.example/mcp", "", "")
}

// A refused write is the row an investigator most wants, and a stale If-Match
// is the one refusal this handler already audits. Recording the ATTEMPTED
// endpoint and refs there is what separates "two admins raced" from "someone
// tried to repoint this server at their own host".
func TestVersionMismatchDenyAuditsTheAttemptedEndpointAndRefs(t *testing.T) {
	t.Setenv("ORBEAT_SECRET_ENV_ALLOW", "")
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	orig, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "a4-stale", Transport: "http",
		EndpointOrCommand: "https://ok.example/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+orig.ID, map[string]any{
		"name": "a4-stale", "transport": "http", "endpointOrCommand": exfilEndpoint,
		"secretRef": "env:ORBEAT_UPSTREAM_TOKEN", "status": "active",
	}, tn)
	req.SetPathValue("id", orig.ID)
	req.Header.Set("If-Match", etag(orig.RowVersion+99)) // stale on purpose
	srv.handleUpdateServer(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412 (body %s)", rec.Code, rec.Body)
	}

	evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	if len(evs) == 0 || evs[0].Action != "server.update" || evs[0].Decision != "deny" {
		t.Fatalf("want a server.update deny audit, got %+v", evs)
	}
	if evs[0].Metadata["reason"] != "version_mismatch" {
		t.Errorf("deny audit lost its reason: %v", evs[0].Metadata)
	}
	// tlsCaRef is absent from the request body above (never mentioned), so
	// under the tri-state PUT contract (defect 1, 2026-09-01) the attempted
	// value recorded here is the literal "(unchanged)" — NOT "": the request
	// never attempted to touch tlsCaRef at all, and recording "" would
	// misname the attempt on the exact surface (audit A4) that exists to
	// describe it accurately. See derefRefOrUnchanged's own doc comment.
	assertServerAuditMetadata(t, evs[0], "a4-stale", exfilEndpoint, "env:ORBEAT_UPSTREAM_TOKEN", "(unchanged)")
}
