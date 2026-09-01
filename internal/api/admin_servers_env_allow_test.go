package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// exfilEndpoint is the other half of audit A4's attack: an absolute external
// https URL, which validEndpoint permits by design and the v1.25.0 dial guard
// deliberately does not refuse (private ranges are this product's primary
// deployment shape). Nothing about the endpoint is what stops the attack, and
// naming it here keeps that visible.
const exfilEndpoint = "https://collector.attacker.tld/mcp"

// TestServerWriteRejectsDisallowedEnvRef is audit A4's write half, driven
// through the real handlers so it also gates the WIRING: api.New's default
// resolver must be the allowlisted one. A test calling
// secrets.NewResolver().ValidateRef directly would pass even if api.New
// installed an unrestricted resolver.
func TestServerWriteRejectsDisallowedEnvRef(t *testing.T) {
	t.Setenv("ORBEAT_SECRET_ENV_ALLOW", "")
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	orig, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "a4-target", Transport: "http",
		EndpointOrCommand: "https://ok.example/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}

	for _, c := range []struct {
		name      string
		secretRef string
		tlsCARef  string
	}{
		{"secretRef names the database url", "env:ORBEAT_DB_URL", ""},
		{"secretRef names an unrelated variable", "env:AWS_SECRET_ACCESS_KEY", ""},
		{"tlsCaRef names the database url", "", "env:ORBEAT_DB_URL"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := map[string]any{
				"name": "a4-" + c.name, "transport": "http",
				"endpointOrCommand": exfilEndpoint, "status": "active",
			}
			if c.secretRef != "" {
				in["secretRef"] = c.secretRef
			}
			if c.tlsCARef != "" {
				in["tlsCaRef"] = c.tlsCARef
			}

			rec := httptest.NewRecorder()
			srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create: status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
			assertNoEcho(t, rec.Body.String(), c.secretRef, c.tlsCARef)

			rec = httptest.NewRecorder()
			req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+orig.ID, in, tn)
			req.SetPathValue("id", orig.ID)
			req.Header.Set("If-Match", etag(orig.RowVersion))
			srv.handleUpdateServer(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("update: status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
			assertNoEcho(t, rec.Body.String(), c.secretRef, c.tlsCARef)
		})
	}

	// Nothing was written. A 400 that still stored the row would leave the
	// gateway to dial it, and the write-time half would be theatre.
	rows, err := st.ListMCPServersByTenant(ctx, tn.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != orig.ID || rows[0].SecretRef != "" {
		t.Fatalf("a refused write reached the store: %+v", rows)
	}
}

// The 400 must say WHICH rule refused it. "invalid secretRef" sends an admin
// looking for a typo in a ref that is perfectly well formed; the fix is either
// renaming the variable or widening ORBEAT_SECRET_ENV_ALLOW, and neither is
// guessable from a blanket message.
func TestDisallowedEnvRefMessageNamesTheAllowlist(t *testing.T) {
	t.Setenv("ORBEAT_SECRET_ENV_ALLOW", "")
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", map[string]any{
		"name": "a4-msg", "transport": "http", "endpointOrCommand": exfilEndpoint,
		"secretRef": "env:ORBEAT_DB_URL", "status": "active",
	}, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	msg := errMessage(t, rec.Body.String())
	for _, want := range []string{"ORBEAT_SECRET_ENV_ALLOW", "ORBEAT_UPSTREAM_"} {
		if !strings.Contains(msg, want) {
			t.Errorf("400 message does not name %q, so an admin cannot act on it: %s", want, msg)
		}
	}
}

// TestServerWriteAllowsAnAllowlistedEnvRef keeps the rule from being vacuously
// strict: without it, refusing every env: ref outright would pass every
// assertion above.
func TestServerWriteAllowsAnAllowlistedEnvRef(t *testing.T) {
	t.Setenv("ORBEAT_SECRET_ENV_ALLOW", "")
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", map[string]any{
		"name": "a4-ok", "transport": "http", "endpointOrCommand": "https://ok.example/mcp",
		"secretRef": "env:ORBEAT_UPSTREAM_TOKEN", "status": "active",
	}, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
}

// An operator list widens the rule for the API too, and it is read where the
// resolver is built. If this fails, the knob exists but no deployment can use
// it.
func TestServerWriteHonoursAnOperatorEnvAllowList(t *testing.T) {
	t.Setenv("ORBEAT_SECRET_ENV_ALLOW", "ACME_MCP_*")
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", map[string]any{
		"name": "a4-acme", "transport": "http", "endpointOrCommand": "https://ok.example/mcp",
		"secretRef": "env:ACME_MCP_TOKEN", "status": "active",
	}, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
}
