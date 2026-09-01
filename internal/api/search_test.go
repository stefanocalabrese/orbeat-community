package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// This file is docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 4's
// HTTP-level gate for ?q=, mirroring sort_order_test.go's discipline for
// ?sort/?order: every assertion drives the real router (newPagingServer/
// pagingGET, paging_test.go), never a handler called directly, so a wiring
// mistake (a list registered without searchParam/refuseSearch wired in) is
// catchable the same way C7 made cursor/limit validation catchable.
//
// The class-defining test is TestListRolesSearchComposesWithPaging: ?q= must
// be applied in SQL, before the keyset predicate and LIMIT, never as a Go
// filter over the returned page. v1.22.0 shipped exactly that bug once
// already for ?state (a filter applied after LIMIT silently drops matching
// rows instead of returning a full page: admin_artifacts.go's
// TestArtifactListStateFilterFullPage is the fix's own pin); this is the same
// class, reproduced for search.

// TestListRolesSearchComposesWithPaging seeds 6 roles, name-ordered,
// alternating matching ("*-hit-*") and non-matching ("*-skip-*"):
// a-skip-1, b-hit-1, c-skip-2, d-hit-2, e-skip-3, f-hit-3. With limit=2, the
// first two rows in NAME order are a-skip-1 and b-hit-1: a buggy
// fetch-then-filter-in-Go implementation would return only ONE matching row
// (b-hit-1) on page 1, a SHORT page, even though 3 matching rows exist
// overall. The correct SQL-side filter returns a FULL page of 2 (b-hit-1,
// d-hit-2), then a final short page of 1 (f-hit-3), walking every matching
// row exactly once and no non-matching row at all.
//
// The search term is deliberately uppercase ("HIT" against lowercase names)
// so this same test also proves case-insensitivity in one pass (?q= uses
// ILIKE, not LIKE, see paging.go's likeSearchArg doc comment for Decision 2
// and its reasoning).
func TestListRolesSearchComposesWithPaging(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)
	names := []string{"a-skip-1", "b-hit-1", "c-skip-2", "d-hit-2", "e-skip-3", "f-hit-3"}
	for _, n := range names {
		if _, err := st.CreateRole(ctx, tn.ID, n); err != nil {
			t.Fatalf("seed role %s: %v", n, err)
		}
	}

	rec := pagingGET(t, srv, tok, "/v1/admin/roles?q=HIT&limit=2")
	next := assertPageEnvelope(t, rec, 2, 2)
	var page1 struct {
		Roles []roleDTO `json:"roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	for i, want := range []string{"b-hit-1", "d-hit-2"} {
		if page1.Roles[i].Name != want {
			t.Fatalf("page1[%d] = %q, want %q: ?q= must run in SQL before LIMIT, not filter the page in Go afterward",
				i, page1.Roles[i].Name, want)
		}
	}
	if next == "" {
		t.Fatalf("a full page of 2 matching rows (3 total match) must carry a nextCursor")
	}

	rec2 := pagingGET(t, srv, tok, "/v1/admin/roles?q=HIT&limit=2&cursor="+next)
	next2 := assertPageEnvelope(t, rec2, 2, 1)
	var page2 struct {
		Roles []roleDTO `json:"roles"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	if page2.Roles[0].Name != "f-hit-3" {
		t.Fatalf("page2[0] = %q, want f-hit-3", page2.Roles[0].Name)
	}
	if next2 != "" {
		t.Fatalf("nextCursor on the final short page = %q, want empty", next2)
	}
}

// TestListRolesSearchEmptyOrAbsentByteIdentical proves an absent ?q= and an
// explicitly-empty "?q=" behave IDENTICALLY, and identically to how this
// route behaved before ?q= existed: searchParam (paging.go) treats both the
// same "" ("no filter", the same meaning likeSearchArg gives an empty
// term), so a client cannot observe any difference between "I never sent q"
// and "I sent q= and cleared it". Bodies are compared byte-for-byte, not
// merely decoded and compared field-by-field, so this would also catch a
// change to key ORDER or whitespace, not just content.
func TestListRolesSearchEmptyOrAbsentByteIdentical(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)
	for _, n := range []string{"role-x", "role-y"} {
		if _, err := st.CreateRole(ctx, tn.ID, n); err != nil {
			t.Fatalf("seed role %s: %v", n, err)
		}
	}

	absent := pagingGET(t, srv, tok, "/v1/admin/roles")
	empty := pagingGET(t, srv, tok, "/v1/admin/roles?q=")
	if absent.Code != http.StatusOK || empty.Code != http.StatusOK {
		t.Fatalf("absent=%d empty=%d, want both 200 (bodies: absent=%s empty=%s)",
			absent.Code, empty.Code, absent.Body, empty.Body)
	}
	if absent.Body.String() != empty.Body.String() {
		t.Fatalf("absent ?q= body:\n%s\nempty ?q= body:\n%s\nmust be byte-identical",
			absent.Body.String(), empty.Body.String())
	}
}

// TestListRolesSearchEscapesWildcards is the wildcard gate: '%' and '_' are
// LIKE/ILIKE metacharacters (any-run-of-characters and any-single-character
// respectively, see paging.go's escapeLikeSpecials doc comment), and a user
// searching for a literal occurrence of either in a name must get exactly
// that literal match, never every row an UNESCAPED wildcard would also
// match. Two independent decoy pairs, one per metacharacter:
//   - "foo_bar" (the literal search target) vs "fooxbar" (what an unescaped
//     '_' would ALSO match, since it stands for any single character between
//     "foo" and "bar").
//   - "100%off" (the literal search target) vs "100xxxoff" (what an
//     unescaped '%' would ALSO match, since it stands for any run of
//     characters between "100" and "off").
//
// A user searching for a literal underscore or percent sign in a name and
// silently matching every row is a real, small, embarrassing bug this test
// exists to keep closed.
func TestListRolesSearchEscapesWildcards(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)
	for _, n := range []string{"foo_bar", "fooxbar", "100%off", "100xxxoff"} {
		if _, err := st.CreateRole(ctx, tn.ID, n); err != nil {
			t.Fatalf("seed role %q: %v", n, err)
		}
	}

	cases := []struct{ term, want string }{
		{"foo_bar", "foo_bar"},
		{"100%off", "100%off"},
	}
	for _, tc := range cases {
		t.Run(tc.term, func(t *testing.T) {
			target := "/v1/admin/roles?limit=100&q=" + url.QueryEscape(tc.term)
			rec := pagingGET(t, srv, tok, target)
			if rec.Code != http.StatusOK {
				t.Fatalf("q=%q = %d, body=%s", tc.term, rec.Code, rec.Body)
			}
			var page struct {
				Roles []roleDTO `json:"roles"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(page.Roles) != 1 || page.Roles[0].Name != tc.want {
				t.Fatalf("q=%q matched %+v, want exactly [%s]: an unescaped wildcard is also matching the decoy row",
					tc.term, page.Roles, tc.want)
			}
		})
	}
}

// TestListServersSearchFiltersInSQL is a lighter, single-page proof for the
// servers list, the same SQL-filter class as
// TestListRolesSearchComposesWithPaging at a smaller scale: ?q= matches only
// the server whose name contains the term, case-insensitively, alongside a
// decoy that would also appear on an unfiltered first page.
func TestListServersSearchFiltersInSQL(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)
	for _, n := range []string{"aaa-decoy", "zzz-target-server"} {
		if _, err := st.CreateMCPServer(ctx, store.MCPServer{
			TenantID: tn.ID, Name: n, Transport: "http",
			EndpointOrCommand: "https://example.invalid/mcp", Status: "active",
		}); err != nil {
			t.Fatalf("seed server %s: %v", n, err)
		}
	}

	rec := pagingGET(t, srv, tok, "/v1/admin/servers?q=TARGET")
	if rec.Code != http.StatusOK {
		t.Fatalf("?q=TARGET = %d, body=%s", rec.Code, rec.Body)
	}
	var page struct {
		Servers []adminServerDTO `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Servers) != 1 || page.Servers[0].Name != "zzz-target-server" {
		t.Fatalf("servers = %+v, want exactly [zzz-target-server]", page.Servers)
	}
}

// TestListArtifactsSearchComposesWithStateFilter proves ?q= and the
// pre-existing ?state filter compose as a SQL-level AND, not as two
// independently-applied narrowings where one silently wins: of four
// artifacts (two named "*-hit-*", two "*-skip-*"; two "draft", two
// "pending"), only the row that is BOTH pending AND name-matching must
// survive ?state=pending&q=hit.
func TestListArtifactsSearchComposesWithStateFilter(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)
	ids := map[string]string{}
	for _, n := range []string{"a-hit-draft", "b-skip-draft", "c-hit-pending", "d-skip-pending"} {
		a, err := st.CreateArtifact(ctx, store.Artifact{
			TenantID: tn.ID, Type: "skill", Name: n,
			Content: "---\nname: " + n + "\ndescription: d\n---\nbody", Visibility: "org",
		})
		if err != nil {
			t.Fatalf("seed artifact %s: %v", n, err)
		}
		ids[n] = a.ID
	}
	for _, n := range []string{"c-hit-pending", "d-skip-pending"} {
		if _, err := st.SetArtifactSubmitted(ctx, tn.ID, ids[n], "alice", []byte("[]"), ""); err != nil {
			t.Fatalf("submit %s: %v", n, err)
		}
	}

	rec := pagingGET(t, srv, tok, "/v1/admin/artifacts?state=pending&q=hit")
	if rec.Code != http.StatusOK {
		t.Fatalf("?state=pending&q=hit = %d, body=%s", rec.Code, rec.Body)
	}
	var page struct {
		Artifacts []artifactDTO `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Artifacts) != 1 || page.Artifacts[0].Name != "c-hit-pending" {
		t.Fatalf("artifacts = %+v, want exactly [c-hit-pending] (both filters must apply together)", page.Artifacts)
	}
}

// TestListEntitlementsSearchRefused and TestListArtifactEntitlementsSearchRefused
// are Task 4 Decision 1's HTTP-level gate: entitlement and
// artifact_entitlement have no natural text column to search (role_id is a
// uuid, see entitlementKeys' doc comment, internal/store/rbac.go, for the
// full reasoning), so ?q= 400s rather than being silently accepted and
// ignored: a search box that appears to filter and does not is worse than
// one that says it cannot (refuseSearch's own comment, paging.go).
//
// Both a non-empty ?q=foo and an explicitly-empty ?q= are refused:
// refuseSearch checks the PARAMETER'S PRESENCE (url.Values.Has), not whether
// its value is non-empty, so a client filtering by an empty string must be
// told this list cannot, the same as one filtering by a real term. An absent
// ?q= is unaffected: the route still lists normally.
func TestListEntitlementsSearchRefused(t *testing.T) {
	srv, _, _, tok := newPagingServer(t)
	for _, q := range []string{"?q=foo", "?q="} {
		t.Run(q, func(t *testing.T) {
			rec := pagingGET(t, srv, tok, "/v1/admin/entitlements"+q)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("entitlements%s = %d, want 400, body=%s", q, rec.Code, rec.Body)
			}
		})
	}
	if rec := pagingGET(t, srv, tok, "/v1/admin/entitlements"); rec.Code != http.StatusOK {
		t.Fatalf("entitlements (no q) = %d, want 200, body=%s", rec.Code, rec.Body)
	}
}

func TestListArtifactEntitlementsSearchRefused(t *testing.T) {
	srv, _, _, tok := newPagingServer(t)
	for _, q := range []string{"?q=foo", "?q="} {
		t.Run(q, func(t *testing.T) {
			rec := pagingGET(t, srv, tok, "/v1/admin/artifact-entitlements"+q)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("artifact-entitlements%s = %d, want 400, body=%s", q, rec.Code, rec.Body)
			}
		})
	}
	if rec := pagingGET(t, srv, tok, "/v1/admin/artifact-entitlements"); rec.Code != http.StatusOK {
		t.Fatalf("artifact-entitlements (no q) = %d, want 200, body=%s", rec.Code, rec.Body)
	}
}
