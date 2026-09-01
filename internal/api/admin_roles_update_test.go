package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// seedRole creates a role with a unique name, returning it.
func seedRole(t *testing.T, st *store.Store, tn store.Tenant) store.Role {
	t.Helper()
	role, err := st.CreateRole(context.Background(), tn.ID, fmt.Sprintf("rename-role-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	return role
}

// putRole drives the real handler exactly the way putEntitlement does for its
// sibling route (admin_entitlement_update_test.go).
func putRole(t *testing.T, srv *Server, tn store.Tenant, id, ifMatch string, in map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := adminReq(context.Background(), http.MethodPut, "/v1/admin/roles/"+id, in, tn)
	req.SetPathValue("id", id)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	srv.handleUpdateRole(rec, req)
	return rec
}

// roleRenameAuditRows returns every role.rename audit event recorded for id,
// split by decision, newest first (ListAuditEventsByTenant's own order).
func roleRenameAuditRows(t *testing.T, st *store.Store, tn store.Tenant, id string) []store.AuditEvent {
	t.Helper()
	evs, err := st.ListAuditEventsByTenant(context.Background(), tn.ID, 100)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var out []store.AuditEvent
	for _, ev := range evs {
		if ev.Action == "role.rename" && ev.Target == id {
			out = append(out, ev)
		}
	}
	return out
}

// TestUpdateRoleAssertionPathRenamesWhenNoLookupConfigured is the baseline
// happy path for a deployment with no realm-role lookup wired
// (newAdminServer's Server never calls SetRoleExistsChecker, matching a
// Community build or an Enterprise one with no ORBEAT_DCR_CLIENT_ID).
func TestUpdateRoleAssertionPathRenamesWhenNoLookupConfigured(t *testing.T) {
	srv, st, tn := newAdminServer(t)
	role := seedRole(t, st, tn)

	rec := putRole(t, srv, tn, role.ID, etag(role.RowVersion), map[string]any{
		"name": "renamed-role", "idpRenamed": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body %s", rec.Code, rec.Body)
	}
	var out roleDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Name != "renamed-role" {
		t.Fatalf("name = %q, want renamed-role", out.Name)
	}
	if out.RowVersion == role.RowVersion {
		t.Fatal("rowVersion did not change after a successful rename")
	}
	if rec.Header().Get("ETag") != etag(out.RowVersion) {
		t.Fatalf("ETag header = %q, want %q", rec.Header().Get("ETag"), etag(out.RowVersion))
	}
	stored, err := st.GetRole(context.Background(), tn.ID, role.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if stored.Name != "renamed-role" {
		t.Fatalf("stored name = %q, want renamed-role", stored.Name)
	}

	// MUTANT 5 test, asserted-success half: the audit metadata must record
	// the MECHANISM as "asserted", not "verified" — the whole governance
	// value of this feature is answering "who said this role existed?", and
	// a handler that always writes "verified" would answer that question
	// with a lie on every deployment that has no lookup configured. Assert
	// the VALUE, not merely that the "idpCheck" key is present (this repo
	// has shipped a test asserting Contains(line, "tenant") that passed on
	// two of three mutants because the key was present while the value
	// stayed empty).
	rows := roleRenameAuditRows(t, st, tn, role.ID)
	var allow *store.AuditEvent
	for i := range rows {
		if rows[i].Decision == "allow" {
			allow = &rows[i]
		}
	}
	if allow == nil {
		t.Fatal("no role.rename allow audit event recorded")
	}
	if got := allow.Metadata["idpCheck"]; got != "asserted" {
		t.Fatalf(`allow audit metadata["idpCheck"] = %v (%T), want the literal string "asserted"`, got, got)
	}
	if got := allow.Metadata["from"]; got != role.Name {
		t.Fatalf(`allow audit metadata["from"] = %v, want %q`, got, role.Name)
	}
	if got := allow.Metadata["to"]; got != "renamed-role" {
		t.Fatalf(`allow audit metadata["to"] = %v, want "renamed-role"`, got)
	}
}

// TestUpdateRoleRefusesWithoutAssertionWhenNoLookupConfigured is the deny half
// of the no-lookup path: an operator who has not ticked the confirmation
// checkbox gets refused, with a machine-readable body the portal can turn
// into that checkbox (docs/plans/orbeat-role-rename-2026-08-27.md's decision:
// "the portal learns the mode from a 400, not a capability endpoint").
func TestUpdateRoleRefusesWithoutAssertionWhenNoLookupConfigured(t *testing.T) {
	srv, st, tn := newAdminServer(t)
	role := seedRole(t, st, tn)

	rec := putRole(t, srv, tn, role.ID, etag(role.RowVersion), map[string]any{"name": "renamed-role"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT without idpRenamed = %d, want 400, body %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != idpAssertionRequiredCode {
		t.Fatalf(`body["code"] = %v, want %q — the portal has no way to recognise this response and offer the checkbox`, body["code"], idpAssertionRequiredCode)
	}

	stored, err := st.GetRole(context.Background(), tn.ID, role.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if stored.Name != role.Name {
		t.Fatalf("a refused rename mutated the role: %q", stored.Name)
	}

	// MUTANT 4 test: the refusal must leave a durable deny trace. Deleting
	// appendDenyAudit's call must turn this red.
	rows := roleRenameAuditRows(t, st, tn, role.ID)
	var deny *store.AuditEvent
	for i := range rows {
		if rows[i].Decision == "deny" {
			deny = &rows[i]
		}
	}
	if deny == nil {
		t.Fatal("no role.rename deny audit event recorded for a refused rename")
	}
	if got := deny.Metadata["idpCheck"]; got != "assertion_missing" {
		t.Fatalf(`deny audit metadata["idpCheck"] = %v, want "assertion_missing"`, got)
	}
}

// TestUpdateRoleLookupConfiguredVerifiesAndRenames is the happy path once a
// realm-role lookup is wired: the assertion field is irrelevant (the lookup
// is authoritative), and the audit records "verified", not "asserted".
func TestUpdateRoleLookupConfiguredVerifiesAndRenames(t *testing.T) {
	srv, st, tn := newAdminServer(t)
	srv.SetRoleExistsChecker(func(_ context.Context, name string) (bool, error) {
		if name != "renamed-role" {
			t.Fatalf("lookup called with %q, want renamed-role", name)
		}
		return true, nil
	})
	role := seedRole(t, st, tn)

	rec := putRole(t, srv, tn, role.ID, etag(role.RowVersion), map[string]any{"name": "renamed-role"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body %s", rec.Code, rec.Body)
	}

	rows := roleRenameAuditRows(t, st, tn, role.ID)
	var allow *store.AuditEvent
	for i := range rows {
		if rows[i].Decision == "allow" {
			allow = &rows[i]
		}
	}
	if allow == nil {
		t.Fatal("no role.rename allow audit event recorded")
	}
	if got := allow.Metadata["idpCheck"]; got != "verified" {
		t.Fatalf(`allow audit metadata["idpCheck"] = %v, want the literal string "verified"`, got)
	}
}

// TestUpdateRoleAssertionCannotBypassAConfiguredLookupSayingNo is MANDATORY
// MUTANT 1, and per the task brief the single most important behaviour in
// this slice: an operator who sets idpRenamed=true must NOT be able to talk
// the API past a realm-role lookup that says the target name does not exist.
// If a mutant lets the assertion short-circuit the lookup, this must go red.
func TestUpdateRoleAssertionCannotBypassAConfiguredLookupSayingNo(t *testing.T) {
	srv, st, tn := newAdminServer(t)
	lookupCalled := false
	srv.SetRoleExistsChecker(func(_ context.Context, name string) (bool, error) {
		lookupCalled = true
		return false, nil
	})
	role := seedRole(t, st, tn)

	rec := putRole(t, srv, tn, role.ID, etag(role.RowVersion), map[string]any{
		"name": "no-such-realm-role", "idpRenamed": true,
	})
	if rec.Code == http.StatusOK {
		t.Fatal("the rename succeeded even though the configured lookup said the role does not exist; " +
			"the operator's assertion bypassed the check")
	}
	if !lookupCalled {
		t.Fatal("the lookup was never called; the assertion short-circuited it before it could run")
	}
	stored, err := st.GetRole(context.Background(), tn.ID, role.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if stored.Name != role.Name {
		t.Fatalf("a refused rename mutated the role to %q", stored.Name)
	}
	rows := roleRenameAuditRows(t, st, tn, role.ID)
	var deny *store.AuditEvent
	for i := range rows {
		if rows[i].Decision == "deny" {
			deny = &rows[i]
		}
	}
	if deny == nil {
		t.Fatal("no role.rename deny audit event recorded")
	}
	if got := deny.Metadata["idpCheck"]; got != "verified_absent" {
		t.Fatalf(`deny audit metadata["idpCheck"] = %v, want "verified_absent"`, got)
	}
}

// TestUpdateRoleLookupErrorNeverDegradesToAssertion is MANDATORY MUTANT 2: a
// 403/401/500/timeout from the identity provider must refuse the rename
// outright, never be silently treated as "the check is unavailable, fall
// back to the operator's word".
func TestUpdateRoleLookupErrorNeverDegradesToAssertion(t *testing.T) {
	srv, st, tn := newAdminServer(t)
	srv.SetRoleExistsChecker(func(_ context.Context, name string) (bool, error) {
		return false, errors.New("keycloak: realm role lookup forbidden: status 403")
	})
	role := seedRole(t, st, tn)

	rec := putRole(t, srv, tn, role.ID, etag(role.RowVersion), map[string]any{
		"name": "renamed-role", "idpRenamed": true,
	})
	if rec.Code == http.StatusOK {
		t.Fatal("the rename succeeded even though the lookup itself errored; " +
			"an unavailable check must never degrade to the assertion path")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("PUT with a failing lookup = %d, want 502", rec.Code)
	}
	stored, err := st.GetRole(context.Background(), tn.ID, role.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if stored.Name != role.Name {
		t.Fatalf("a refused rename mutated the role to %q", stored.Name)
	}
	rows := roleRenameAuditRows(t, st, tn, role.ID)
	var deny *store.AuditEvent
	for i := range rows {
		if rows[i].Decision == "deny" {
			deny = &rows[i]
		}
	}
	if deny == nil {
		t.Fatal("no role.rename deny audit event recorded for a lookup failure")
	}
	if got := deny.Metadata["idpCheck"]; got != "lookup_error" {
		t.Fatalf(`deny audit metadata["idpCheck"] = %v, want "lookup_error"`, got)
	}
}

// TestUpdateRoleRequiresIfMatch is MANDATORY MUTANT 3, mirroring
// TestUpdateEntitlementRequiresIfMatch's contract exactly: absent is 428,
// stale is 412, neither mutates, and a valid one still works.
func TestUpdateRoleRequiresIfMatch(t *testing.T) {
	srv, st, tn := newAdminServer(t)
	role := seedRole(t, st, tn)

	if rec := putRole(t, srv, tn, role.ID, "", map[string]any{"name": "x", "idpRenamed": true}); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match = %d, want 428", rec.Code)
	}
	if rec := putRole(t, srv, tn, role.ID, `"999"`, map[string]any{"name": "x", "idpRenamed": true}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match = %d, want 412", rec.Code)
	}
	stored, err := st.GetRole(context.Background(), tn.ID, role.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if stored.Name != role.Name {
		t.Fatalf("a refused update mutated the role: %q", stored.Name)
	}
	if rec := putRole(t, srv, tn, role.ID, etag(role.RowVersion), map[string]any{"name": "y", "idpRenamed": true}); rec.Code != http.StatusOK {
		t.Fatalf("valid If-Match = %d, want 200, body %s", rec.Code, rec.Body)
	}
}

// TestUpdateRoleRefusesRenameToKeycloakBuiltinNames is audit B8's rename-half
// red-proof, and the one the finding calls "the sharpest part": renaming a
// role to a Keycloak built-in name must be refused BEFORE the realm-role
// lookup ever runs, because a configured lookup would answer "verified" for
// "offline_access" (Keycloak genuinely has that role) and wave the
// escalation through exactly as described. The lookup is wired here and its
// own call is observed, so a regression that moves this check to AFTER
// verifyIdpRename turns this red rather than merely returning the same
// status code for a different reason.
func TestUpdateRoleRefusesRenameToKeycloakBuiltinNames(t *testing.T) {
	srv, st, tn := newAdminServer(t)
	lookupCalled := false
	srv.SetRoleExistsChecker(func(_ context.Context, name string) (bool, error) {
		lookupCalled = true
		return true, nil // a real Keycloak would say exactly this for "offline_access"
	})

	for _, name := range []string{"offline_access", "uma_authorization"} {
		t.Run(name, func(t *testing.T) {
			lookupCalled = false
			role := seedRole(t, st, tn)

			rec := putRole(t, srv, tn, role.ID, etag(role.RowVersion), map[string]any{
				"name": name, "idpRenamed": true,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("rename to %q = %d, want 400, body %s", name, rec.Code, rec.Body)
			}
			if lookupCalled {
				t.Fatalf("the realm-role lookup was called for %q; the built-in refusal must run BEFORE "+
					"verifyIdpRename, since a real Keycloak answers \"verified\" for this exact name "+
					"(B8's own asymmetry)", name)
			}
			stored, err := st.GetRole(context.Background(), tn.ID, role.ID)
			if err != nil {
				t.Fatalf("reread: %v", err)
			}
			if stored.Name != role.Name {
				t.Fatalf("a refused rename mutated the role to %q", stored.Name)
			}
			rows := roleRenameAuditRows(t, st, tn, role.ID)
			if len(rows) != 0 {
				t.Fatalf("a role.rename audit event was recorded for the refused rename to %q: %+v", name, rows)
			}
		})
	}
}

// TestUpdateRoleNameCollisionIs409 pins store.ErrNameTaken's mapping.
func TestUpdateRoleNameCollisionIs409(t *testing.T) {
	srv, st, tn := newAdminServer(t)
	role := seedRole(t, st, tn)
	other := seedRole(t, st, tn)

	rec := putRole(t, srv, tn, role.ID, etag(role.RowVersion), map[string]any{
		"name": other.Name, "idpRenamed": true,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("colliding rename = %d, want 409, body %s", rec.Code, rec.Body)
	}
	stored, err := st.GetRole(context.Background(), tn.ID, role.ID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if stored.Name != role.Name {
		t.Fatalf("a refused rename mutated the role to %q", stored.Name)
	}
}
