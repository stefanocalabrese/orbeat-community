package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// This file is docs/plans/orbeat-admin-search-sort-2026-08-27.md Task 3's
// HTTP-level gate for ?sort/?order: an unknown value of either must 400
// (never a silent fallback to the default, the plan's own reason: a table
// sorted one way while its header, or the ?sort the client just sent, claims
// another), the one allowlisted ?sort value plus both directions must NOT
// 400, and at least one list's ?order=desc must be proven correct end to end
// through the REAL router (not just at the store layer, which
// internal/store/paging_desc_test.go already covers), the same "assert
// through the door production opens" discipline paging_test.go's
// newPagingServer doc comment states for cursor/limit validation. Virtual
// keys are Enterprise-only and covered separately in
// admin_virtual_keys.ee_test.go.

// assertSortOrderValidation400s proves ?sort/?order validation for the list
// at path, whose one allowlisted ?sort value (internal/api/paging.go) is
// wantSort, through the real router: an unrecognized column or direction
// 400s, and the allowed column combined with each valid direction (absent,
// "asc", "desc") returns 200, a version of sortOrderParams that rejected
// its OWN allowed value would break every list's default request, so that
// direction is asserted here too, not just the rejection paths.
func assertSortOrderValidation400s(t *testing.T, srv *Server, tok, path, wantSort string) {
	t.Helper()
	badCases := []struct {
		name  string
		query string
	}{
		{"unknown sort column", "?sort=bogus"},
		{"unknown order direction", "?order=sideways"},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := pagingGET(t, srv, tok, path+tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s%s = %d, want 400, body=%s", path, tc.query, rec.Code, rec.Body)
			}
		})
	}
	for _, order := range []string{"", "asc", "desc"} {
		q := "?sort=" + wantSort
		if order != "" {
			q += "&order=" + order
		}
		t.Run("allowed sort with order="+order, func(t *testing.T) {
			rec := pagingGET(t, srv, tok, path+q)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s%s = %d, want 200, body=%s", path, q, rec.Code, rec.Body)
			}
		})
	}
}

func TestListRolesSortOrderValidation(t *testing.T) {
	srv, _, _, tok := newPagingServer(t)
	assertSortOrderValidation400s(t, srv, tok, "/v1/admin/roles", roleSortName)
}

func TestListServersSortOrderValidation(t *testing.T) {
	srv, _, _, tok := newPagingServer(t)
	assertSortOrderValidation400s(t, srv, tok, "/v1/admin/servers", mcpServerSortName)
}

func TestListEntitlementsSortOrderValidation(t *testing.T) {
	srv, _, _, tok := newPagingServer(t)
	assertSortOrderValidation400s(t, srv, tok, "/v1/admin/entitlements", entitlementSortName)
}

func TestListArtifactEntitlementsSortOrderValidation(t *testing.T) {
	srv, _, _, tok := newPagingServer(t)
	assertSortOrderValidation400s(t, srv, tok, "/v1/admin/artifact-entitlements", artifactEntitlementSortName)
}

func TestListArtifactsSortOrderValidation(t *testing.T) {
	srv, _, _, tok := newPagingServer(t)
	assertSortOrderValidation400s(t, srv, tok, "/v1/admin/artifacts", artifactSortName)
}

// TestListRolesOrderDescViaHTTP is this file's end-to-end proof: ?order=desc
// on GET /v1/admin/roles, through the real router, actually reverses the
// returned order, not merely accepted as a valid parameter (the validation
// tests above), but WIRED all the way from the query string to
// store.ListRolesPage's desc argument and store.RoleCursor's matching Sort
// identity.
func TestListRolesOrderDescViaHTTP(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)
	for _, n := range []string{"role-a", "role-b", "role-c"} {
		if _, err := st.CreateRole(ctx, tn.ID, n); err != nil {
			t.Fatalf("seed role %s: %v", n, err)
		}
	}

	rec := pagingGET(t, srv, tok, "/v1/admin/roles?order=desc&limit=100")
	if rec.Code != http.StatusOK {
		t.Fatalf("list ?order=desc = %d, body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Roles []roleDTO `json:"roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"role-c", "role-b", "role-a"}
	if len(got.Roles) != len(want) {
		t.Fatalf("got %d roles (%+v), want %d", len(got.Roles), got.Roles, len(want))
	}
	for i, w := range want {
		if got.Roles[i].Name != w {
			t.Errorf("position %d = %q, want %q, ?order=desc must reach the store layer, not be accepted and ignored", i, got.Roles[i].Name, w)
		}
	}
}

// TestListRolesOrderDescPaginatesAcrossPages is TestListRolesOrderDescViaHTTP
// plus a page boundary: ?order=desc combined with a real ?cursor round trip
// (encode -> decode -> replay) must keep walking descending, not silently
// reset to ascending or refuse the cursor as a mismatch (the direction the
// cursor is minted under, via store.RoleCursor's desc argument threaded from
// this same request's ?order, must equal the direction the next request asks
// for).
func TestListRolesOrderDescPaginatesAcrossPages(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)
	for _, n := range []string{"role-a", "role-b", "role-c", "role-d"} {
		if _, err := st.CreateRole(ctx, tn.ID, n); err != nil {
			t.Fatalf("seed role %s: %v", n, err)
		}
	}

	var got []string
	next := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("pagination did not terminate after 20 pages of limit=2 over 4 roles")
		}
		q := "?order=desc&limit=2"
		if next != "" {
			q += "&cursor=" + next
		}
		rec := pagingGET(t, srv, tok, "/v1/admin/roles"+q)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: %d, body=%s", pages, rec.Code, rec.Body)
		}
		var env struct {
			Roles      []roleDTO `json:"roles"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode page %d: %v", pages, err)
		}
		if len(env.Roles) == 0 {
			break
		}
		for _, r := range env.Roles {
			got = append(got, r.Name)
		}
		next = env.NextCursor
		if next == "" {
			break
		}
	}

	want := []string{"role-d", "role-c", "role-b", "role-a"}
	if len(got) != len(want) {
		t.Fatalf("walked %d roles (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d = %q, want %q", i, got[i], w)
		}
	}
}
