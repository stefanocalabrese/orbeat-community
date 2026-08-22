package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/secrets"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// The resolver must default NON-NIL. A nil field guarded by a nil-check would
// make validation silently skip whenever the server was constructed without
// wiring it — a check that cannot fire, which is the failure mode this whole
// slice exists to remove.
func TestNewDefaultsSecretsResolverNonNil(t *testing.T) {
	s := New(nil, nil, nil, nil, nil)
	if s.secrets == nil {
		t.Fatal("api.New left s.secrets nil; validation would silently skip")
	}
	if err := s.secrets.ValidateRef("valut:kv/x#t"); err == nil {
		t.Fatal("default resolver accepted an unregistered scheme")
	}
}

// Mirrors TestSetScannerOverridesAndIgnoresNil for the resolver seam. The
// nil-ignore half is the point: it is what stops New's non-nil default from
// being wiped, and until now only a comment asserted it. Identity comparison is
// the available discriminator — Resolver's provider map is unexported, so a
// narrower resolver cannot be built from this package.
func TestSetSecretsOverridesAndIgnoresNil(t *testing.T) {
	srv := New(nil, nil, nil, nil, nil)
	def := srv.secrets
	if def == nil {
		t.Fatal("New should install a default resolver")
	}
	custom := secrets.NewResolver()
	if custom == def {
		t.Fatal("test cannot discriminate: NewResolver returned the same pointer")
	}
	srv.SetSecrets(custom)
	if srv.secrets != custom {
		t.Fatal("SetSecrets should override the default")
	}
	srv.SetSecrets(nil) // nil must be ignored, not wipe the resolver
	if srv.secrets != custom {
		t.Fatal("SetSecrets(nil) should be ignored, not wipe the resolver")
	}
}

func TestValidEndpoint(t *testing.T) {
	cases := []struct {
		endpoint string
		want     bool
	}{
		// http/sse: absolute http(s), a real hostname, no userinfo.
		{"http://upstream:8000", true},
		{"https://mcp.example.com/x", true},
		{"HTTP://h/x", true},        // url.Parse lowercases the scheme
		{"http://[::1]:8080", true}, // IPv6 literal
		{"http://host?x@y", true},   // '@' is in the query; User is nil
		{"https://sse.example.com/v1", true},

		{"/mcp", false},               // relative
		{"upstream:8000", false},      // absolute but wrong scheme
		{"http://", false},            // no host
		{"http:///mcp", false},        // no host
		{"http://:8080", false},       // BARE PORT: Host is ":8080" (non-empty!), Hostname is ""
		{"http://:8080/mcp", false},   // same
		{"ftp://user:pw@h/x", false},  // wrong scheme
		{"http://user:pw@h/x", false}, // userinfo
		{"http://%75ser@h/x", false},  // percent-encoded userinfo
		{"h ttp://x", false},
		// Ports outside 1-65535 parse fine and pass a scheme+hostname check, but
		// nothing can ever dial them — the "server with zero tools and no
		// explanation" outcome this validation exists to prevent.
		{"http://h:0/", false},
		{"http://h:65536/", false},
		{"http://h:99999/", false},
		{"http://h:65535/", true},
		{"http://h:1/", true}, // parse error
	}
	for _, c := range cases {
		if got := validEndpoint(c.endpoint); got != c.want {
			t.Errorf("validEndpoint(%q) = %v, want %v", c.endpoint, got, c.want)
		}
	}
}

// Both write paths must reject the same malformed input, and NEITHER may echo the
// submitted value. secretRef is where an operator eventually pastes a raw secret
// instead of a reference — echoing it would copy the credential into the response
// body and the request log.
func TestServerWriteRejectsMalformedRefAndEndpoint(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	// A pre-existing, valid row to aim the PUT at, so the update path reaches
	// validation rather than 404-ing on a missing id.
	orig, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "val-target", Transport: "http",
		EndpointOrCommand: "https://ok.example/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}

	cases := []struct {
		name      string
		transport string
		endpoint  string
		secretRef string
	}{
		{"unregistered scheme", "http", "https://x", "valut:kv/mcp#token"},
		{"ref without scheme", "http", "https://x", "ghp_examplerawsecretvalue"},
		{"vault ref without field", "http", "https://x", "vault://gh"},
		{"vault ref empty field", "http", "https://x", "vault:kv/mcp#"},
		{"relative endpoint", "http", "/mcp", ""},
		{"bare port endpoint", "http", "http://:8080", ""},
		{"unroutable port 0", "http", "http://h:0/", ""},
		{"unroutable port 65536", "http", "http://h:65536/", ""},
		{"control char in ref", "http", "https://x", "env:A\x00B"},
		{"endpoint with userinfo", "http", "http://user:pw@h/x", ""},
		{"wrong scheme with userinfo", "http", "ftp://user:pw@h/x", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := map[string]any{
				"name": "val-" + c.name, "transport": c.transport,
				"endpointOrCommand": c.endpoint, "status": "active",
			}
			if c.secretRef != "" {
				in["secretRef"] = c.secretRef
			}

			// create
			rec := httptest.NewRecorder()
			srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create: status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
			assertNoEcho(t, rec.Body.String(), c.secretRef, c.endpoint)

			// update — full-replace, must validate identically. None of these
			// cases ever reach the store (they all 400 at validateServerWrite),
			// so orig's row_version never advances and its captured value
			// stays correct across the whole table.
			rec = httptest.NewRecorder()
			req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+orig.ID, in, tn)
			req.SetPathValue("id", orig.ID)
			req.Header.Set("If-Match", etag(orig.RowVersion))
			srv.handleUpdateServer(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("update: status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
			assertNoEcho(t, rec.Body.String(), c.secretRef, c.endpoint)
		})
	}
}

// The 400 must name the SPECIFIC value-free reason, not one blanket message: an
// admin who typos the scheme and one who omits a vault #field have different
// problems and different fixes. Every error ValidateRef can return is a static
// literal (the operator-input interpolation in internal/secrets is in Resolve,
// not ValidateRef), so surfacing it is safe — and this test is what stops a
// "simplification" back to a single undifferentiated string from passing.
//
// The vault-specific locator-shape rows moved to
// admin_servers_validation.ee_test.go's TestServerWriteSurfacesSpecificRefReasonEnterprise:
// vault: is an Enterprise-only scheme (docs/specs/2026-08-19-orbeat-
// community-repo-generation-design.md §4), not registered by NewResolver in
// a generated Community tree. Both share assertServerWriteSurfacesRefReason.
func TestServerWriteSurfacesSpecificRefReason(t *testing.T) {
	assertServerWriteSurfacesRefReason(t, []refReasonCase{
		{"unregistered scheme", "valut:kv/x#t", "unknown ref scheme"},
		// Asserted WITH the "secrets: " prefix on purpose. A bare
		// `must be "<scheme>:<locator>"` is also a substring of the plausible
		// api-layer blanket message, so it could not tell a surfaced error from a
		// discarded one — a row that cannot fail for the reason it exists.
		{"no scheme prefix", "ghp_examplerawsecretvalue", `secrets: ref must be "<scheme>:<locator>"`},
	})
}

// refReasonCase is TestServerWriteSurfacesSpecificRefReason and
// TestServerWriteSurfacesSpecificRefReasonEnterprise's shared case shape.
type refReasonCase struct {
	name      string
	secretRef string
	wantSub   string
}

// assertServerWriteSurfacesRefReason is the shared body both tests run
// against their own (disjoint) case sets.
func assertServerWriteSurfacesRefReason(t *testing.T, cases []refReasonCase) {
	t.Helper()
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := map[string]any{
				"name": "gran-" + c.name, "transport": "http",
				"endpointOrCommand": "https://ok.example/mcp", "status": "active",
				"secretRef": c.secretRef,
			}
			rec := httptest.NewRecorder()
			srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
			// Assert on the DECODED message: the JSON encoder HTML-escapes
			// '<'/'>', so "<mount>/<path>" never appears literally on the wire
			// even though the client receives it.
			if got := errMessage(t, rec.Body.String()); !strings.Contains(got, c.wantSub) {
				t.Errorf("message %q does not surface the specific reason %q", got, c.wantSub)
			}
			assertNoEcho(t, rec.Body.String(), c.secretRef)
		})
	}
}

// Validation must run BEFORE checkServerSlugCollision, so a purely local
// rejection costs no DB round-trip. Both the code comment and the commit message
// claim that ordering; without this case, swapping the two if-blocks would break
// no test. A name that collides AND a malformed endpoint must yield the 400, not
// the collision's 409.
func TestServerWriteValidatesBeforeSlugCollisionQuery(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	if _, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "Order Target", Transport: "http",
		EndpointOrCommand: "https://ok.example/mcp", Status: "active",
	}); err != nil {
		t.Fatalf("seed server: %v", err)
	}

	// "order-target" slugifies onto the seeded "Order Target", so the collision
	// check WOULD 409 this — if it ran first.
	in := map[string]any{
		"name": "order-target", "transport": "http",
		"endpointOrCommand": "http://:8080", "status": "active",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (validation must precede the slug-collision query); body %s",
			rec.Code, rec.Body)
	}
}

// TestAdminUpdateServerIfMatchPrecedesSlugCollisionQuery is
// TestServerWriteValidatesBeforeSlugCollisionQuery's concern one guard
// earlier, for update: If-Match must be parsed BEFORE checkServerSlugCollision
// ever queries the store, so a request missing the precondition never even
// reaches the DB and never gets a chance to leak a 409 instead of a 428.
//
// The code comment in handleUpdateServer already claims this ordering ("parsed
// before touching the store … without any read or write"), but nothing pinned
// it: moving the ifMatch(r) call below decodeJSONOrFail, validateServerWrite
// AND checkServerSlugCollision left the whole package green, including every
// other test in this file — a comment asserting an invariant that could not
// fail. A name that collides AND a missing If-Match must yield 428, not 409.
func TestAdminUpdateServerIfMatchPrecedesSlugCollisionQuery(t *testing.T) {
	ctx := context.Background()
	srv, st, tn := newAdminServer(t)

	if _, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "Order Target", Transport: "http",
		EndpointOrCommand: "https://ok.example/mcp", Status: "active",
	}); err != nil {
		t.Fatalf("seed server: %v", err)
	}
	victim, err := st.CreateMCPServer(ctx, store.MCPServer{
		TenantID: tn.ID, Name: "order-target-victim", Transport: "http",
		EndpointOrCommand: "https://ok2.example/mcp", Status: "active",
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}

	// "order-target" slugifies onto the seeded "Order Target", so the
	// collision check WOULD 409 this — if it ran before the If-Match parse.
	in := map[string]any{
		"name": "order-target", "transport": "http",
		"endpointOrCommand": "https://ok3.example/mcp", "status": "active",
	}
	rec := httptest.NewRecorder()
	req := adminReq(ctx, http.MethodPut, "/v1/admin/servers/"+victim.ID, in, tn)
	req.SetPathValue("id", victim.ID)
	// Deliberately NO If-Match header.
	srv.handleUpdateServer(rec, req)
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428 (If-Match must be checked before the slug-collision query); body %s",
			rec.Code, rec.Body)
	}
}

// errMessage decodes the standard {"error":{"message":...}} envelope. It falls
// back to the raw body so a malformed envelope still produces a readable failure
// rather than an empty string that could pass an assertion vacuously.
func errMessage(t *testing.T, body string) string {
	t.Helper()
	var env struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error.Message == "" {
		t.Errorf("response is not the standard error envelope: %s", body)
		return body
	}
	return env.Error.Message
}

// assertNoEcho fails if the response contains any submitted value.
//
// It checks the raw body AND the decoded message. Raw alone would be a false
// NEGATIVE for any value the JSON encoder escapes (it HTML-escapes '<', '>',
// '&') — the wrong direction to be wrong in for a leak assertion, since the
// client still decodes the value back out.
//
// Note that a bare "<scheme>:" ref would trip this spuriously: the provider's
// own value-free message legitimately names the scheme ("secrets/env: empty
// variable name" contains "env:"). The scheme is public; the locator is the
// sensitive part — internal/secrets' own test is shape-aware for exactly this
// reason. Do not add such a row here.
func assertNoEcho(t *testing.T, body string, values ...string) {
	t.Helper()
	decoded := body
	var env struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err == nil && env.Error.Message != "" {
		decoded = env.Error.Message
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		if strings.Contains(body, v) || strings.Contains(decoded, v) {
			t.Errorf("error body echoes the submitted value %q: %s", v, body)
		}
	}
}

// A well-formed server must still write, so the rule cannot be vacuously strict.
// Uses an env: ref rather than vault: — the scheme is incidental to what
// this test checks (see TestServerWriteAcceptsWellFormedEnterprise,
// admin_servers_validation.ee_test.go, for the vault-specific case: vault:
// is Enterprise-only, docs/specs/2026-08-19-orbeat-community-repo-
// generation-design.md §4).
func TestServerWriteAcceptsWellFormed(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	in := map[string]any{
		"name": "val-ok", "transport": "http",
		"endpointOrCommand": "http://upstream:8000",
		"secretRef":         "env:GITHUB_TOKEN", "status": "active",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
}

// TestServerWriteValidatesTLSCARef proves tlsCaRef goes through the same
// structural ref validation as secretRef: an unregistered scheme is rejected, a
// registered one is accepted, and empty is accepted (the field is optional).
// Validation is STRUCTURAL only — it does not resolve the ref or check the bytes
// parse as a certificate, which would need I/O at write time (spec §4).
func TestServerWriteValidatesTLSCARef(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ref    string
		wantOK bool
	}{
		{"empty is allowed", "", true},
		{"registered scheme", "env:INTERNAL_CA", true},
		{"unregistered scheme", "valut:pki/ca#pem", false},
		{"no scheme", "just-a-string", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := validateServerWrite(secrets.NewResolver(), "https://h/x", "", tc.ref)
			if ok != tc.wantOK {
				t.Fatalf("validateServerWrite(tlsCaRef=%q) ok = %v, want %v (msg %q)", tc.ref, ok, tc.wantOK, msg)
			}
		})
	}
}

// TestCreateServerRejectsBadTLSCARef proves the handler wires tlsCaRef into
// validateServerWrite. The unit test above exercises the validator directly and
// would still pass if the handler never passed the field in.
func TestCreateServerRejectsBadTLSCARef(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	in := map[string]any{
		"name": "tls-bad", "transport": "http",
		"endpointOrCommand": "https://ok.example/mcp", "status": "active",
		"tlsCaRef": "valut:pki/ca#pem",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	assertNoEcho(t, rec.Body.String(), "valut:pki/ca#pem")
}

// TestCreateServerPersistsTLSCARef proves a valid ref survives the handler and
// comes back as hasTlsCa on the read path, WITHOUT echoing the locator.
func TestCreateServerPersistsTLSCARef(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	in := map[string]any{
		"name": "tls-good", "transport": "http",
		"endpointOrCommand": "https://ok.example/mcp", "status": "active",
		"tlsCaRef": "env:INTERNAL_CA",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create response has no id: %+v", created)
	}

	rec = httptest.NewRecorder()
	getReq := adminReq(ctx, http.MethodGet, "/v1/admin/servers/"+id, nil, tn)
	getReq.SetPathValue("id", id)
	srv.handleGetServer(rec, getReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); strings.Contains(got, "env:INTERNAL_CA") {
		t.Fatalf("get response echoes the raw tlsCaRef locator: %s", got)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got["hasTlsCa"] != true {
		t.Fatalf("hasTlsCa = %v, want true: %+v", got["hasTlsCa"], got)
	}
	if _, leaked := got["tlsCaRef"]; leaked {
		t.Fatal("get response leaked tlsCaRef")
	}
}

// stdio is no longer an accepted transport: orbeat-gateway is a REMOTE broker and
// its transport switch (internal/gateway/broker.go) cannot dial a local stdio
// subprocess, and no other channel consumes catalog servers. A stdio row was
// therefore accepted, listed in the catalog, and silently yielded zero tools —
// exactly the failure this validation slice exists to prevent.
func TestServerWriteRejectsStdioTransport(t *testing.T) {
	ctx := context.Background()
	srv, _, tn := newAdminServer(t)

	in := map[string]any{
		"name": "val-stdio", "transport": "stdio",
		"endpointOrCommand": "npx -y @modelcontextprotocol/server-github", "status": "active",
	}
	rec := httptest.NewRecorder()
	srv.handleCreateServer(rec, adminReq(ctx, http.MethodPost, "/v1/admin/servers", in, tn))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stdio accepted: status = %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if got := errMessage(t, rec.Body.String()); !strings.Contains(got, "http, sse") {
		t.Errorf("message should name the allowed transports; got %q", got)
	}
}
