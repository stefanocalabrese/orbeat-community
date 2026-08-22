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

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

func TestPageParamsDefaultsAndClamp(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantLimit int
		wantErr   bool
	}{
		{"absent uses default", "", 100, false},
		{"explicit under max", "?limit=25", 25, false},
		{"at max", "?limit=500", 500, false},
		{"above max clamps, not an error", "?limit=9999", 500, false},
		{"zero rejected", "?limit=0", 0, true},
		{"negative rejected", "?limit=-1", 0, true},
		{"non-integer rejected", "?limit=abc", 0, true},
		{"empty value uses default", "?limit=", 100, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/admin/roles"+tc.query, nil)
			limit, cursor, err := pageParams(r, defaultListLimit, maxListLimit, cursorShape{cursorText})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("limit=%q: want error, got limit=%d", tc.query, limit)
				}
				return
			}
			if err != nil {
				t.Fatalf("limit=%q: unexpected error %v", tc.query, err)
			}
			if limit != tc.wantLimit {
				t.Errorf("limit = %d, want %d", limit, tc.wantLimit)
			}
			if cursor != nil {
				t.Errorf("cursor = %+v, want nil (none supplied)", cursor)
			}
		})
	}
}

// TestCursorRoundTripsColonBearingKey is spec section 4.3: role names are
// validated only as non-empty, so a sort key CAN contain the audit cursor's
// ':' delimiter. The JSON encoding must round-trip it exactly. Driven through
// pageParams (not decodeListCursor directly, per C7) since that is the door
// every list handler will call in Task 7/8.
//
// What this does not prove: nothing about limit parsing (covered above) or
// any of the malformed-input rejection paths (covered below).
func TestCursorRoundTripsColonBearingKey(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	in := store.ListCursor{Keys: []string{`a:b:"c",d`}, ID: id}
	enc := encodeListCursor(in)

	r := httptest.NewRequest("GET", "/v1/admin/roles?cursor="+enc, nil)
	_, out, err := pageParams(r, defaultListLimit, maxListLimit, cursorShape{cursorText})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out == nil || len(out.Keys) != 1 || out.Keys[0] != in.Keys[0] || out.ID != in.ID {
		t.Fatalf("round-trip = %+v, want %+v: a colon in a sort key must survive", out, in)
	}
}

// TestCursorRejectsMalformed drives pageParams (the door every list handler
// calls) with a battery of malformed cursors across all three cursorKinds. It
// proves each malformed shape is rejected as a validationError (which fail()
// maps to 400), not left to reach a Postgres cast and surface as a 500. It
// does not itself assert an HTTP status: pageParams returns a Go error, not a
// response; the error-to-400 mapping is fail()'s own contract, exercised by
// whichever handler calls this in Task 7/8.
//
// The two "overflows int4" cases are C5's gap one cursorKind over (found in
// review): strconv.Atoi validates against Go's int (64-bit on every release
// target), but the only cursorInt list — artifact_revision.revision_num — is
// a Postgres int4, and 3000000000/-3000000000 both fit in a 64-bit Go int
// while overflowing int4 in both directions. Left unvalidated, either value
// reaches artifact_revision's `$n::int` cursor placeholder and raises
// SQLSTATE 22003 (numeric_value_out_of_range), which — like 22021 above — is
// in neither idCastNotFound (22P02 only) nor fail()'s switch, so it would
// surface as a 500 instead of this test's required 400.
func TestCursorRejectsMalformed(t *testing.T) {
	cases := []struct {
		name   string
		cursor string
		shape  cursorShape
	}{
		{"not base64", "!!!not-base64!!!", cursorShape{cursorText}},
		{"not json", "bm90LWpzb24", cursorShape{cursorText}},
		{"wrong key count", encodeListCursor(store.ListCursor{Keys: []string{"a", "b"}, ID: "11111111-2222-3333-4444-555555555555"}), cursorShape{cursorText}},
		{"id not a uuid", encodeListCursor(store.ListCursor{Keys: []string{"a"}, ID: "not-a-uuid"}), cursorShape{cursorText}},
		{"uuid key not a uuid", encodeListCursor(store.ListCursor{Keys: []string{"nope"}, ID: "11111111-2222-3333-4444-555555555555"}), cursorShape{cursorUUID}},
		{"int key not an int", encodeListCursor(store.ListCursor{Keys: []string{"nope"}, ID: "11111111-2222-3333-4444-555555555555"}), cursorShape{cursorInt}},
		{"int key overflows int4", encodeListCursor(store.ListCursor{Keys: []string{"3000000000"}, ID: "11111111-2222-3333-4444-555555555555"}), cursorShape{cursorInt}},
		{"int key underflows int4", encodeListCursor(store.ListCursor{Keys: []string{"-3000000000"}, ID: "11111111-2222-3333-4444-555555555555"}), cursorShape{cursorInt}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/admin/x?cursor="+tc.cursor, nil)
			if _, _, err := pageParams(r, defaultListLimit, maxListLimit, tc.shape); err == nil {
				t.Fatalf("cursor %q was accepted; a malformed cursor must be a 400 validationError, not a Postgres cast failure surfacing as 500", tc.cursor)
			}
		})
	}
}

// TestCursorRejectsNULByteInTextKey is correction C5. A NUL byte survives
// encoding/json (raw invalid UTF-8 does NOT: it is coerced to the Unicode
// replacement character, which Postgres accepts, so that path is not
// reachable here), reaches the $n::text placeholder, and raises SQLSTATE
// 22021, which neither idCastNotFound (22P02 only) nor fail()'s switch maps,
// so it would land in default: and return HTTP 500. Three of the six lists
// (mcp_server.name, role.name, artifact.type/name) use text keys, so this is
// reachable in production.
//
// Verified live before writing this test, not assumed: encoding a Go string
// containing a NUL byte with json.Marshal produces the six-character JSON
// escape for the null code point, and json.Unmarshal decodes that escape back
// to a real NUL byte. The value genuinely round-trips through the codec
// unmodified rather than being stripped or replaced, so a cursor carrying one
// really does reach decodeListCursor's text-key case with the NUL intact.
//
// Driven through pageParams, not decodeListCursor directly (C7): that is the
// door every list handler will call, and this is the case the plan's original
// "any byte sequence is a valid text key" comment got wrong.
func TestCursorRejectsNULByteInTextKey(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	enc := encodeListCursor(store.ListCursor{Keys: []string{"a\x00b"}, ID: id})

	r := httptest.NewRequest("GET", "/v1/admin/roles?cursor="+enc, nil)
	if _, _, err := pageParams(r, defaultListLimit, maxListLimit, cursorShape{cursorText}); err == nil {
		t.Fatalf("cursor with a NUL byte in a text key was accepted; it must be a 400 validationError, not a Postgres 22021 surfacing as a 500")
	}
}

// ---- Task 7: the five non-artifact list handlers, end to end ----

// newPagingServer builds a Server wired with a REAL auth.Validator (backed by
// an in-process test IdP — mwOrderTestIdP, defined in middleware_order_test.go
// and reused here rather than duplicated) and returns an admin bearer token.
//
// Every assertion below drives requests through srv.Handler().ServeHTTP, never
// a handler function called directly: per plan correction C7 ("assert through
// the door production opens"), a test that bypasses routing and middleware
// cannot catch a wiring mistake — e.g. a list handler registered under the
// wrong HTTP method, or the admin-role gate missing from one route. A nil
// validator (as newAdminServer/newArtifactServer use) cannot serve this: an
// admin route's wrapper calls s.validator.RequireAuth at Handler()-build time,
// and a real request would nil-deref inside it.
func newPagingServer(t *testing.T) (*Server, *store.Store, store.Tenant, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)
	tenantName := fmt.Sprintf("paging-%d", time.Now().UnixNano())
	tn, err := st.GetOrCreateTenantByName(ctx, tenantName)
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	idp := newMWOrderTestIdP(t)
	v, err := auth.NewValidator(ctx, auth.Config{Issuer: idp.srv.URL, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	srv := New(st, authz.NewResolver(st, tenantName), v, nil, nil)
	tok := idp.token(t, "kc-paging-admin", []string{"orbeat-admin"})
	return srv, st, tn, tok
}

// pagingGET issues an authenticated GET against the real router (auth + RBAC
// + resolver middleware all run — see newPagingServer's C7 note above).
func pagingGET(t *testing.T, srv *Server, tok, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// assertListValidation400s proves, through the real router, that a
// non-positive/non-integer ?limit and a malformed ?cursor each 400 for path.
// This is the HTTP-reachable counterpart to TestPageParamsDefaultsAndClamp /
// TestCursorRejectsMalformed above, which stop at pageParams' Go error and
// never prove fail() actually turns it into a 400 for a real request.
func assertListValidation400s(t *testing.T, srv *Server, tok, path string) {
	t.Helper()
	cases := []struct {
		name  string
		query string
	}{
		{"limit zero", "?limit=0"},
		{"limit negative", "?limit=-3"},
		{"limit non-integer", "?limit=abc"},
		{"cursor malformed", "?cursor=not-a-valid-cursor!!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := pagingGET(t, srv, tok, path+tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s%s = %d, want 400, body=%s", path, tc.query, rec.Code, rec.Body)
			}
		})
	}
}

// assertPageEnvelope decodes rec as one page of a paginated list response and
// asserts the two envelope-level invariants every list must satisfy: the
// response 200s, it echoes back the ACTUAL post-clamp limit (a client sending
// limit=9999 must see 500 come back, not 9999 or the default — that echo is
// what lets a client tell a full page from a short one without hard-coding
// the server's clamp), and this page carries exactly wantRows rows. It
// returns nextCursor for the caller to drive the next iteration.
//
// It works across every list's differently-named rows key
// (roles/servers/entitlements/artifactEntitlements/revisions) and
// differently-typed DTO without needing either parameterized: the envelope
// has exactly one array-valued top-level key besides the two scalar fields,
// so this finds it structurally rather than by name. One implementation
// shared by every walk test below (and the exact-multiple test), so adding a
// sixth list's walk test is one call, not a sixth copy of this logic — Task 7
// review found only the roles walk test asserted `limit` at all; the other
// four decoded it into nothing. Callers still decode the body a second time
// into their own typed slice for row-level (id/revision) duplicate/skip
// checks, which need the real DTO type this helper deliberately doesn't know.
func assertPageEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantLimit, wantRows int) (nextCursor string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("page fetch = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}

	limRaw, ok := env["limit"]
	if !ok {
		t.Fatalf(`envelope missing "limit": %s`, rec.Body)
	}
	var limit int
	if err := json.Unmarshal(limRaw, &limit); err != nil {
		t.Fatalf("decode limit: %v", err)
	}
	if limit != wantLimit {
		t.Fatalf("limit echoed = %d, want %d (the post-clamp value)", limit, wantLimit)
	}

	curRaw, ok := env["nextCursor"]
	if !ok {
		t.Fatalf(`envelope missing "nextCursor": %s`, rec.Body)
	}
	if err := json.Unmarshal(curRaw, &nextCursor); err != nil {
		t.Fatalf("decode nextCursor: %v", err)
	}

	rowsKey, rowCount := "", -1
	for k, raw := range env {
		if k == "limit" || k == "nextCursor" {
			continue
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			continue // scalar field, not the rows array
		}
		rowsKey, rowCount = k, len(rows)
	}
	if rowCount == -1 {
		t.Fatalf("envelope has no array-valued rows key: %s", rec.Body)
	}
	if rowCount != wantRows {
		t.Fatalf("%s = %d rows, want %d", rowsKey, rowCount, wantRows)
	}
	return nextCursor
}

// TestListRolesPagination walks GET /v1/admin/roles two rows at a time
// through the real router and proves the walk is exhaustive and exact: every
// seeded role is seen exactly once, in cursor order, and the final short
// page's nextCursor is empty. Also proves a bad limit/cursor 400s through the
// same door.
//
// What this class of test (repeated per-handler below, this comment not
// repeated) does not prove: behavior under concurrent writes mid-walk (a
// keyset cursor is a snapshot of "strictly after this key"; a row inserted
// behind an already-passed position is invisible to the rest of THIS walk —
// accepted keyset-pagination behavior, not a defect this test is checking
// for); the underlying SQL plan/index usage (internal/store's job); or the
// exact-multiple-page heuristic, which is pinned once, separately, by
// TestListRolesPaginationExactMultiplePage (5 rows is not a multiple of the
// page size 2, so this walk never exercises that boundary).
func TestListRolesPagination(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	const n, limit = 5, 2
	seeded := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		r, err := st.CreateRole(ctx, tn.ID, fmt.Sprintf("role-%02d", i))
		if err != nil {
			t.Fatalf("seed role %d: %v", i, err)
		}
		seeded[r.ID] = true
	}

	assertListValidation400s(t, srv, tok, "/v1/admin/roles")

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatal("pagination did not terminate")
		}
		target := fmt.Sprintf("/v1/admin/roles?limit=%d", limit)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := pagingGET(t, srv, tok, target)
		wantRows := min(limit, n-len(seen))
		next := assertPageEnvelope(t, rec, limit, wantRows)

		var body struct {
			Roles []roleDTO `json:"roles"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, r := range body.Roles {
			if seen[r.ID] {
				t.Fatalf("role %+v seen twice across pages — duplicate row at a page boundary", r)
			}
			seen[r.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for id := range seeded {
		if !seen[id] {
			t.Fatalf("role %s was never returned by any page", id)
		}
	}
	if len(seen) != len(seeded) {
		t.Fatalf("saw %d roles across all pages, want exactly %d", len(seen), len(seeded))
	}
}

// TestListRolesPaginationExactMultiplePage pins the nextCursor heuristic
// documented on handleListRoles and its four siblings: len(rows) == limit
// means "maybe more" — never "definitely more", because the handler never
// issues a LIMIT+1 lookahead query. When the tenant's true row count is an
// EXACT MULTIPLE of the page size, the client is handed a nextCursor on the
// technically-last full page, and must spend one extra round-trip (an empty
// page with an empty nextCursor) to learn the walk is actually over. That is
// standard keyset-pagination behavior and the contract this API ships — not a
// bug, and not something a later reader should "fix" by adding a lookahead
// query. Pinned once, here, rather than in every per-handler walk test above.
//
// What this does not prove: that the four sibling handlers hit the same
// boundary the same way (roles was picked arbitrarily; all five share the
// identical len(rows)==limit heuristic in the handler code, so one pin is
// sufficient) or anything about duplicate/skip detection across a
// non-unique sort key (TestListEntitlementsPagination /
// TestListArtifactEntitlementsPagination's job).
func TestListRolesPaginationExactMultiplePage(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	const limit, n = 2, 4 // n is an exact multiple of limit
	for i := 0; i < n; i++ {
		if _, err := st.CreateRole(ctx, tn.ID, fmt.Sprintf("exact-role-%02d", i)); err != nil {
			t.Fatalf("seed role %d: %v", i, err)
		}
	}

	cursor := ""
	var totalSeen int
	for page := 0; page < n/limit; page++ {
		target := fmt.Sprintf("/v1/admin/roles?limit=%d", limit)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := pagingGET(t, srv, tok, target)
		next := assertPageEnvelope(t, rec, limit, limit)
		if next == "" {
			t.Fatalf("page %d: nextCursor empty on a FULL page — every full page must carry a cursor under this heuristic, even the technically-last one", page)
		}
		totalSeen += limit
		cursor = next
	}
	if totalSeen != n {
		t.Fatalf("saw %d roles across full pages, want %d", totalSeen, n)
	}

	// One more fetch with the last full page's cursor: the "extra empty page"
	// the heuristic costs. It must come back empty with an EMPTY nextCursor —
	// that is the client's actual walk-termination signal.
	rec := pagingGET(t, srv, tok, fmt.Sprintf("/v1/admin/roles?limit=%d&cursor=%s", limit, cursor))
	trailing := assertPageEnvelope(t, rec, limit, 0)
	if trailing != "" {
		t.Fatalf("trailing empty page's nextCursor = %q, want empty (this is the walk-termination signal)", trailing)
	}
}

// TestListServersPagination is TestListRolesPagination's counterpart for
// GET /v1/admin/servers (cursor shape {cursorText} on server name, like
// roles). See TestListRolesPagination's doc comment for what this proves and
// does not prove.
func TestListServersPagination(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	const n, limit = 5, 2
	seeded := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		m, err := st.CreateMCPServer(ctx, store.MCPServer{
			TenantID: tn.ID, Name: fmt.Sprintf("server-%02d", i),
			Transport: "http", EndpointOrCommand: "https://example.test/x", Status: "active",
		})
		if err != nil {
			t.Fatalf("seed server %d: %v", i, err)
		}
		seeded[m.ID] = true
	}

	assertListValidation400s(t, srv, tok, "/v1/admin/servers")

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatal("pagination did not terminate")
		}
		target := fmt.Sprintf("/v1/admin/servers?limit=%d", limit)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := pagingGET(t, srv, tok, target)
		wantRows := min(limit, n-len(seen))
		next := assertPageEnvelope(t, rec, limit, wantRows)

		var body struct {
			Servers []adminServerDTO `json:"servers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, m := range body.Servers {
			if seen[m.ID] {
				t.Fatalf("server %+v seen twice across pages", m)
			}
			seen[m.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for id := range seeded {
		if !seen[id] {
			t.Fatalf("server %s was never returned by any page", id)
		}
	}
	if len(seen) != len(seeded) {
		t.Fatalf("saw %d servers across all pages, want exactly %d", len(seen), len(seeded))
	}
}

// TestListEntitlementsPagination is TestListRolesPagination's counterpart for
// GET /v1/admin/entitlements (cursor shape {cursorUUID} on role_id — NOT a
// unique key: every seeded entitlement here shares the same role_id, so a
// duplicate/skip at a page boundary would show up as a false positive only if
// the (role_id, id) tiebreaker were dropped — exactly the spec-flagged trap).
// See TestListRolesPagination's doc comment for what this proves and does not.
func TestListEntitlementsPagination(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	role, err := st.CreateRole(ctx, tn.ID, "ent-role")
	if err != nil {
		t.Fatalf("seed role: %v", err)
	}

	const n, limit = 5, 2
	seeded := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		m, err := st.CreateMCPServer(ctx, store.MCPServer{
			TenantID: tn.ID, Name: fmt.Sprintf("ent-server-%02d", i),
			Transport: "http", EndpointOrCommand: "https://example.test/x", Status: "active",
		})
		if err != nil {
			t.Fatalf("seed server %d: %v", i, err)
		}
		e, err := st.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: m.ID})
		if err != nil {
			t.Fatalf("seed entitlement %d: %v", i, err)
		}
		seeded[e.ID] = true
	}

	assertListValidation400s(t, srv, tok, "/v1/admin/entitlements")

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatal("pagination did not terminate")
		}
		target := fmt.Sprintf("/v1/admin/entitlements?limit=%d", limit)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := pagingGET(t, srv, tok, target)
		wantRows := min(limit, n-len(seen))
		next := assertPageEnvelope(t, rec, limit, wantRows)

		var body struct {
			Entitlements []entitlementDTO `json:"entitlements"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, e := range body.Entitlements {
			if seen[e.ID] {
				t.Fatalf("entitlement %+v seen twice across pages — this is exactly the non-unique-role_id skip/duplicate trap", e)
			}
			seen[e.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for id := range seeded {
		if !seen[id] {
			t.Fatalf("entitlement %s was never returned by any page", id)
		}
	}
	if len(seen) != len(seeded) {
		t.Fatalf("saw %d entitlements across all pages, want exactly %d", len(seen), len(seeded))
	}
}

// TestListArtifactEntitlementsPagination is TestListRolesPagination's
// counterpart for GET /v1/admin/artifact-entitlements (cursor shape
// {cursorUUID} on role_id — also non-unique, mirroring entitlements). See
// TestListRolesPagination's doc comment for what this proves and does not.
func TestListArtifactEntitlementsPagination(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	role, err := st.CreateRole(ctx, tn.ID, "ae-role")
	if err != nil {
		t.Fatalf("seed role: %v", err)
	}

	const n, limit = 5, 2
	seeded := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		a, err := st.CreateArtifact(ctx, store.Artifact{
			TenantID: tn.ID, Type: "skill", Name: fmt.Sprintf("ae-skill-%02d", i),
			Content: "---\nname: x\ndescription: d\n---\nbody", Visibility: "role",
		})
		if err != nil {
			t.Fatalf("seed artifact %d: %v", i, err)
		}
		e, err := st.CreateArtifactEntitlement(ctx, store.ArtifactEntitlement{TenantID: tn.ID, RoleID: role.ID, ArtifactID: a.ID})
		if err != nil {
			t.Fatalf("seed artifact entitlement %d: %v", i, err)
		}
		seeded[e.ID] = true
	}

	assertListValidation400s(t, srv, tok, "/v1/admin/artifact-entitlements")

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatal("pagination did not terminate")
		}
		target := fmt.Sprintf("/v1/admin/artifact-entitlements?limit=%d", limit)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := pagingGET(t, srv, tok, target)
		wantRows := min(limit, n-len(seen))
		next := assertPageEnvelope(t, rec, limit, wantRows)

		var body struct {
			ArtifactEntitlements []artifactEntitlementDTO `json:"artifactEntitlements"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, e := range body.ArtifactEntitlements {
			if seen[e.ID] {
				t.Fatalf("artifact entitlement %+v seen twice across pages", e)
			}
			seen[e.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for id := range seeded {
		if !seen[id] {
			t.Fatalf("artifact entitlement %s was never returned by any page", id)
		}
	}
	if len(seen) != len(seeded) {
		t.Fatalf("saw %d artifact entitlements across all pages, want exactly %d", len(seen), len(seeded))
	}
}

// TestListArtifactRevisionsPagination is TestListRolesPagination's
// counterpart for GET /v1/admin/artifacts/{id}/revisions. Two ways this list
// differs from the other four: it sorts newest-first (revision_num DESC, the
// pre-existing behavior this task must preserve), and it takes its scoping id
// from the URL PATH rather than a query filter — so this test also proves the
// pre-existing ErrNotFound contract (an unknown artifact id 404s, not 500 or
// an empty 200) survives being routed through the now-paginated handler. See
// TestListRolesPagination's doc comment for what this class of test proves
// and does not prove more generally.
// TestListArtifactRevisionsPagination moved to
// admin_artifact_review_paging.ee_test.go: revision history is
// Enterprise-only (docs/specs/2026-08-19-orbeat-community-repo-generation-
// design.md §4).

// ---- Task 8: GET /v1/admin/artifacts (?state in SQL, ?include=content) ----

// TestArtifactListSlimByDefault is Task 8's headline correction (spec C2): the
// list is slim by default (the four heavy payload columns —
// content/memorySeed/approvedContent/approvedMemorySeed — are omitted unless
// ?include=content is set), and critically `approved` must still report the
// real has_approved column rather than a derivation off the now-blank
// approvedContent field. A regression on that second point is silent: it
// doesn't 500 or 400, it just reports every listed artifact as unapproved,
// vanishing the portal's Live badge.
//
// What this does not prove: the ?state filter (TestArtifactListStateFilterFullPage's
// job), ?include's own value validation (TestArtifactListIncludeParamValidation's
// job — this test only exercises the two values that pass validation),
// pagination walking (Task 7's tests already cover the shared
// pageParams/cursor machinery this handler reuses unchanged), anything about
// get-by-id (GET /v1/admin/artifacts/{id}, which is unpaginated and always
// full — admin_artifacts_test.go's job), or sort-key ordering across a type
// boundary in the two-key (type, name) cursor — this test seeds a single
// type, so it cannot catch a mis-ordered/dropped second key
// (TestArtifactListPaginatesAcrossTypeBoundary's job).
func TestArtifactListSlimByDefault(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	a, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "subagent", Name: "slim-subagent",
		Description: "d",
		Content:     "---\nname: slim-subagent\ndescription: d\n---\nBODY-SENTINEL",
		MemoryScope: "user",
		MemorySeed:  "SEED-SENTINEL",
		Visibility:  "org",
	})
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	// Driven directly at the store layer, not through the submit/approve HTTP
	// handlers (Enterprise-only, admin_artifact_review.ee.go): this test is
	// about the LIST endpoint's slim/full projection, not the approval
	// workflow, and SetArtifactSubmitted/SetArtifactApproved are the shared
	// store primitives Community's own future auto-approve path will also
	// call (docs/specs/2026-08-19-orbeat-community-repo-generation-design.md §4).
	if _, err := st.SetArtifactSubmitted(ctx, tn.ID, a.ID, "alice", []byte("[]")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, _, err := st.SetArtifactApproved(ctx, tn.ID, a.ID, "bob", 0); err != nil {
		t.Fatalf("approve: %v", err) // gives it a live approved snapshot
	}

	// Slim: no ?include.
	rec := pagingGET(t, srv, tok, "/v1/admin/artifacts")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "BODY-SENTINEL") {
		t.Fatalf("slim list raw body must not contain content/approvedContent: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "SEED-SENTINEL") {
		t.Fatalf("slim list raw body must not contain memorySeed/approvedMemorySeed: %s", rec.Body)
	}
	var slim struct {
		Artifacts []artifactDTO `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &slim); err != nil {
		t.Fatalf("decode slim: %v", err)
	}
	if len(slim.Artifacts) != 1 {
		t.Fatalf("want 1 artifact, got %d: %+v", len(slim.Artifacts), slim.Artifacts)
	}
	got := slim.Artifacts[0]
	if got.Content != "" {
		t.Fatalf("slim content = %q, want empty", got.Content)
	}
	if got.MemorySeed != "" {
		t.Fatalf("slim memorySeed = %q, want empty", got.MemorySeed)
	}
	if got.ApprovedContent != "" {
		t.Fatalf("slim approvedContent = %q, want empty", got.ApprovedContent)
	}
	if got.ApprovedMemorySeed != "" {
		t.Fatalf("slim approvedMemorySeed = %q, want empty", got.ApprovedMemorySeed)
	}
	// C2: approved must reflect has_approved, not a derivation off the
	// (now-blank) approvedContent — this is the assertion the red-proof in
	// Step 4 targets.
	if !got.Approved {
		t.Fatalf("approved = false on a slim row with a live approved snapshot; the portal's Live badge would silently vanish for every listed artifact")
	}

	// Full: ?include=content restores all four heavy fields.
	rec2 := pagingGET(t, srv, tok, "/v1/admin/artifacts?include=content")
	if rec2.Code != http.StatusOK {
		t.Fatalf("list include=content = %d, body %s", rec2.Code, rec2.Body)
	}
	if !strings.Contains(rec2.Body.String(), "BODY-SENTINEL") {
		t.Fatalf("include=content raw body must contain content/approvedContent: %s", rec2.Body)
	}
	if !strings.Contains(rec2.Body.String(), "SEED-SENTINEL") {
		t.Fatalf("include=content raw body must contain memorySeed/approvedMemorySeed: %s", rec2.Body)
	}
	var full struct {
		Artifacts []artifactDTO `json:"artifacts"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode full: %v", err)
	}
	if len(full.Artifacts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(full.Artifacts))
	}
	fa := full.Artifacts[0]
	if !strings.Contains(fa.Content, "BODY-SENTINEL") {
		t.Fatalf("full content = %q, want BODY-SENTINEL", fa.Content)
	}
	if fa.MemorySeed != "SEED-SENTINEL" {
		t.Fatalf("full memorySeed = %q, want SEED-SENTINEL", fa.MemorySeed)
	}
	if !strings.Contains(fa.ApprovedContent, "BODY-SENTINEL") {
		t.Fatalf("full approvedContent = %q, want BODY-SENTINEL", fa.ApprovedContent)
	}
	if fa.ApprovedMemorySeed != "SEED-SENTINEL" {
		t.Fatalf("full approvedMemorySeed = %q, want SEED-SENTINEL", fa.ApprovedMemorySeed)
	}
	if !fa.Approved {
		t.Fatalf("approved = false on the full row, want true")
	}
}

// TestArtifactListStateFilterFullPage proves the ?state filter runs in SQL,
// not a Go loop applied after the LIMIT: 3 pending artifacts are interleaved
// with 3 drafts BY NAME (the list's sort key, alongside type) so that the
// first two rows in cursor order are one draft then one pending — a filter
// applied after the page query would see only ONE pending row in that first
// page of two fetched rows, returning a SHORT page where SQL-side filtering
// returns a FULL one. limit=2 with 3 total pending rows also exercises the
// nextCursor "possibly more" walk across a real ?state boundary.
//
// What this does not prove: the slim/full projection switch
// (TestArtifactListSlimByDefault's job), duplicate/skip detection across a
// page boundary on a non-unique sort key (type/name here IS the artifact's
// unique-enough key — see artifactKeys in internal/store/artifact.go — so
// that class of trap, covered for role_id in Task 7's entitlement tests,
// doesn't apply to this list), or the ?state filter's interaction with a
// type boundary — every seed here is type "skill" on purpose, to keep this
// test's only variable the approval state; the (type, name) ordering itself
// is TestArtifactListPaginatesAcrossTypeBoundary's job, deliberately kept
// orthogonal (per C7 rule 4: each test proves one thing).
func TestArtifactListStateFilterFullPage(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	// All type "skill" so name is the sort tiebreaker: a < b < c < d < e < f,
	// alternating draft/pending.
	names := []string{"a-draft", "b-pending", "c-draft", "d-pending", "e-draft", "f-pending"}
	ids := make(map[string]string, len(names))
	for _, name := range names {
		a, err := st.CreateArtifact(ctx, store.Artifact{
			TenantID: tn.ID, Type: "skill", Name: name,
			Content: "---\nname: " + name + "\ndescription: d\n---\nbody", Visibility: "org",
		})
		if err != nil {
			t.Fatalf("seed artifact %s: %v", name, err)
		}
		ids[name] = a.ID
	}
	// Driven directly at the store layer — see TestArtifactListSlimByDefault's
	// comment on why: this test is about the ?state filter, not the approval
	// workflow's HTTP surface.
	for _, name := range []string{"b-pending", "d-pending", "f-pending"} {
		if _, err := st.SetArtifactSubmitted(ctx, tn.ID, ids[name], "alice", []byte("[]")); err != nil {
			t.Fatalf("submit %s: %v", name, err)
		}
	}

	assertListValidation400s(t, srv, tok, "/v1/admin/artifacts")

	rec := pagingGET(t, srv, tok, "/v1/admin/artifacts?state=pending&limit=2")
	next := assertPageEnvelope(t, rec, 2, 2)
	var page1 struct {
		Artifacts []artifactDTO `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	for _, a := range page1.Artifacts {
		if a.ApprovalState != "pending" {
			t.Fatalf("state filter leaked a non-pending row into the page: %+v", a)
		}
	}
	if next == "" {
		t.Fatalf("a full page of 2 pending rows (3 total pending exist) must carry a nextCursor")
	}

	rec2 := pagingGET(t, srv, tok, "/v1/admin/artifacts?state=pending&limit=2&cursor="+next)
	next2 := assertPageEnvelope(t, rec2, 2, 1)
	var page2 struct {
		Artifacts []artifactDTO `json:"artifacts"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	if page2.Artifacts[0].ApprovalState != "pending" {
		t.Fatalf("remaining row not pending: %+v", page2.Artifacts[0])
	}
	if page2.Artifacts[0].Name != "f-pending" {
		t.Fatalf("remaining row = %q, want f-pending", page2.Artifacts[0].Name)
	}
	if next2 != "" {
		t.Fatalf("nextCursor on the final short page = %q, want empty", next2)
	}
}

// TestArtifactListIncludeParamValidation is the Task 8 review's IMPORTANT-1
// fix: ?include is a query param introduced by THIS handler (unlike the
// pre-existing, deliberately-left-lenient ?state), so rejecting an
// unrecognized value is not a breaking change — and the house convention,
// verified rather than assumed against handleListAudit's ?format
// (admin_audit.go: absent -> "json" default, "json"/"csv" accepted,
// anything else -> 400) and parseAuditBound's ?from/?to, is exactly this:
// default the absent value, reject the unrecognized one. Left lenient,
// "?include=Content" (a case typo) or "?include=foo" would silently return
// blank content with a 200 — the same silent-blank-content failure shape as
// the C8 portal regressions.
//
// What this does not prove: anything about the slim/full field content
// itself (TestArtifactListSlimByDefault's job) — this test only checks the
// HTTP status code for each ?include value.
func TestArtifactListIncludeParamValidation(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	if _, err := st.CreateArtifact(ctx, store.Artifact{
		TenantID: tn.ID, Type: "skill", Name: "inc-check",
		Content: "---\nname: inc-check\ndescription: d\n---\nbody", Visibility: "org",
	}); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	cases := []struct {
		name    string
		query   string
		wantErr bool
	}{
		{"absent uses the slim default", "", false},
		{"content is accepted", "?include=content", false},
		{"wrong case rejected", "?include=Content", true},
		{"unrecognized value rejected", "?include=foo", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := pagingGET(t, srv, tok, "/v1/admin/artifacts"+tc.query)
			if tc.wantErr {
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("include%s = %d, want 400, body=%s", tc.query, rec.Code, rec.Body)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("include%s = %d, want 200, body=%s", tc.query, rec.Code, rec.Body)
			}
		})
	}
}

// TestArtifactListPaginatesAcrossTypeBoundary is the Task 8 review's
// MINOR-2 addition: the artifact list is the codebase's first genuine
// two-key cursor (type, name; every other list in Task 7 sorts on one key).
// Both TestArtifactListSlimByDefault and TestArtifactListStateFilterFullPage
// seed a single type, so neither can catch a sort-key mis-ordering across
// types — a SWAPPED ArtifactCursor key order would still happen to sort
// correctly within one type, and only shows up once two types are present
// and the walk crosses the type boundary mid-list. This test seeds two
// types with interleaved-by-name rows (so "row order equals insertion
// order" would also fail this test, not just "row order equals name order")
// and walks with a page size that does not evenly divide either type's row
// count, asserting the exact (type, name) ascending order holds across the
// boundary itself.
//
// What this does not prove: a third type (two is sufficient to prove type is
// the primary sort key — a third would only repeat the same assertion) or
// the ?state filter's interaction with the type boundary, deliberately left
// to TestArtifactListStateFilterFullPage's single-type setup (C7 rule 4:
// each test proves one thing).
func TestArtifactListPaginatesAcrossTypeBoundary(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	// "skill" < "subagent" lexicographically, so this also proves type is the
	// PRIMARY key: if name were compared first, "m-skill" (created 2nd) and
	// "m-subagent" (created 5th) would collide in the middle of the walk in
	// the wrong order relative to their neighbors.
	type row struct{ typ, name string }
	seeds := []row{
		{"skill", "a-skill"},
		{"skill", "m-skill"},
		{"skill", "z-skill"},
		{"subagent", "a-subagent"},
		{"subagent", "m-subagent"},
		{"subagent", "z-subagent"},
	}
	for _, sd := range seeds {
		if _, err := st.CreateArtifact(ctx, store.Artifact{
			TenantID: tn.ID, Type: sd.typ, Name: sd.name,
			Content: "---\nname: " + sd.name + "\ndescription: d\n---\nbody", Visibility: "org",
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", sd.typ, sd.name, err)
		}
	}

	const limit = 4 // does not evenly divide either type's 3-row count
	var walked []row
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > len(seeds) {
			t.Fatal("pagination did not terminate")
		}
		target := fmt.Sprintf("/v1/admin/artifacts?limit=%d", limit)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := pagingGET(t, srv, tok, target)
		var body struct {
			Artifacts  []artifactDTO `json:"artifacts"`
			NextCursor string        `json:"nextCursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, a := range body.Artifacts {
			walked = append(walked, row{a.Type, a.Name})
		}
		if body.NextCursor == "" {
			break
		}
		cursor = body.NextCursor
	}

	want := []row{
		{"skill", "a-skill"}, {"skill", "m-skill"}, {"skill", "z-skill"},
		{"subagent", "a-subagent"}, {"subagent", "m-subagent"}, {"subagent", "z-subagent"},
	}
	if len(walked) != len(want) {
		t.Fatalf("walked %d rows across all pages, want %d: %+v", len(walked), len(want), walked)
	}
	for i, w := range want {
		if walked[i] != w {
			t.Fatalf("row %d = %+v, want %+v — full walked order: %+v", i, walked[i], w, walked)
		}
	}
}
