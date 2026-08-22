package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// userDeleteRequest issues an authenticated DELETE against the real router,
// mirroring roleDeleteRequest (admin_test.go).
func userDeleteRequest(t *testing.T, srv *Server, tok, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/users/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestAdminDeleteUserReportsIdentityAndAudits is Task 2's mandatory
// red-proof target: the response reports the identity DeleteUser destroyed,
// and the audit metadata names it too. Dropping either the handler's
// userDeleteResponse fields or its Metadata map must fail exactly the
// assertions checking that half, leaving the other green: proving the two
// are independently covered rather than one accidentally implying the other.
//
// Unlike TestAdminDeleteRoleCascadesAndAudits, there is no cascade to seed or
// check: a user row owns nothing else in the schema (see
// store.DeletedUser's doc comment), so this test's shape differs from its
// role counterpart on purpose, not by omission.
func TestAdminDeleteUserReportsIdentityAndAudits(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	u, err := st.UpsertUser(ctx, store.User{
		TenantID: tn.ID, Subject: "kc-del-user", Email: "del@x.io", DisplayName: "Delete Me",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	rec := userDeleteRequest(t, srv, tok, u.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete user = %d, want 200, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Subject     string `json:"subject"`
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v, raw=%s", err, rec.Body)
	}
	if body.Subject != "kc-del-user" || body.Email != "del@x.io" || body.DisplayName != "Delete Me" {
		t.Fatalf("body = %+v, want the deleted identity", body)
	}

	// The row is actually gone.
	if _, err := st.GetUserBySubject(ctx, tn.ID, "kc-del-user"); err == nil {
		t.Fatal("expected the user row to be gone after delete")
	}

	// Exactly one audit row for THIS action (the admin token's own resolve
	// upserts a second user row for "kc-paging-admin" earlier in the request
	// chain, but that is a store-level UpsertUser, not an audited mutation,
	// so it must not appear here).
	evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("audit events = %d, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Action != "user.delete" || ev.Decision != "allow" {
		t.Fatalf("audit event = %+v, want action=user.delete decision=allow", ev)
	}
	if ev.Metadata["subject"] != "kc-del-user" {
		t.Errorf("metadata[subject] = %v, want %q", ev.Metadata["subject"], "kc-del-user")
	}
	if ev.Metadata["email"] != "del@x.io" {
		t.Errorf("metadata[email] = %v, want %q", ev.Metadata["email"], "del@x.io")
	}
	if ev.Metadata["displayName"] != "Delete Me" {
		t.Errorf("metadata[displayName] = %v, want %q", ev.Metadata["displayName"], "Delete Me")
	}
}

// TestAdminDeleteUserNotFound covers all three shapes that must 404,
// mirroring TestAdminDeleteRoleNotFound: unknown id, malformed id, and a
// cross-tenant id (a real user, but in a different tenant). The cross-tenant
// case also proves the foreign user still exists afterwards.
func TestAdminDeleteUserNotFound(t *testing.T) {
	ctx := context.Background()
	srv, st, _, tok := newPagingServer(t)

	other, err := st.GetOrCreateTenantByName(ctx, fmt.Sprintf("del-user-other-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	foreign, err := st.UpsertUser(ctx, store.User{TenantID: other.ID, Subject: "kc-foreign-user", Email: "f@x.io"})
	if err != nil {
		t.Fatalf("upsert foreign user: %v", err)
	}

	for _, tc := range []struct{ name, id string }{
		{"unknown uuid", "00000000-0000-0000-0000-000000000000"},
		{"malformed id", "not-a-uuid"},
		{"cross-tenant id", foreign.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := userDeleteRequest(t, srv, tok, tc.id)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("delete user (%s) = %d, want 404, body %s", tc.name, rec.Code, rec.Body)
			}
		})
	}

	if _, err := st.GetUserBySubject(ctx, other.ID, "kc-foreign-user"); err != nil {
		t.Fatalf("expected the foreign user to still exist after the cross-tenant 404, got err=%v", err)
	}
}
