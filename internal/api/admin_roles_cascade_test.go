package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedRoleCascadeChildren inserts one row into each of the three child tables
// of role that the role.delete audit record was silent about until A10, plus
// the mcp_server row usage_daily's composite FK needs.
//
// Raw SQL, not CreateVirtualKey / CreateRoleQuota / IncrementUsage, and the
// reason is the edition boundary rather than convenience: those three
// constructors live in store's *.ee.go files, so naming them from a shared
// _test.go file would stop the generated Community tree compiling
// (internal/communitygen drops every *.ee.go). handleDeleteRole and
// store.RevokedGrants are SHARED code that both editions run, and the three
// tables exist in both editions because migrations are never edition-split,
// so the gate over what that handler writes has to run in both too.
func seedRoleCascadeChildren(t *testing.T, tenantID, roleID string, clientIDs []string, monthlyCalls *int64) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	defer pool.Close()

	var serverID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO mcp_server (tenant_id, name, transport, endpoint_or_command, status)
		VALUES ($1, 'cascade-usage-srv', 'http', 'https://example.invalid/mcp', 'active')
		RETURNING id::text`, tenantID).Scan(&serverID); err != nil {
		t.Fatalf("seed mcp_server: %v", err)
	}
	for _, cid := range clientIDs {
		if _, err := pool.Exec(ctx, `
			INSERT INTO virtual_key (tenant_id, client_id, role_id, name)
			VALUES ($1, $2, $3, $4)`, tenantID, cid, roleID, "key "+cid); err != nil {
			t.Fatalf("seed virtual_key %s: %v", cid, err)
		}
	}
	if monthlyCalls != nil {
		if _, err := pool.Exec(ctx, `
			INSERT INTO role_quota (tenant_id, role_id, monthly_calls)
			VALUES ($1, $2, $3)`, tenantID, roleID, *monthlyCalls); err != nil {
			t.Fatalf("seed role_quota: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO usage_daily (tenant_id, day, subject, server_id, tool, role_id, calls)
		VALUES ($1, DATE '2026-08-01', 'orbeat-vk-ci', $2, 'read', $3, 9),
		       ($1, DATE '2026-08-02', 'orbeat-vk-ci', $2, 'read', $3, 4)`,
		tenantID, serverID, roleID); err != nil {
		t.Fatalf("seed usage_daily: %v", err)
	}
}

// TestAdminDeleteRoleAuditsEveryCascadingChild is A10 at the surface the
// finding is actually about. TestAdminDeleteRoleCascadesAndAudits (admin_test.go)
// already asserts the two children that were always reported; this asserts the
// three that were not, and the one that matters most is virtualKeyClientIds.
//
// Deleting a role destroys its virtual keys outright. Every CI job holding one
// fails on its next tools/call, and the Keycloak client behind it is orphaned:
// the orbeat row is GONE rather than revoked, so the orphan query
// virtual_key.ee.go points operators at (GET /v1/admin/virtual-keys?revoked=true)
// cannot return it. This metadata list is the only place those client_ids
// survive at all, which is why the assertion is on the SET of strings and not
// on a count.
func TestAdminDeleteRoleAuditsEveryCascadingChild(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	role, err := st.CreateRole(ctx, tn.ID, "cascade-audit-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	quota := int64(25000)
	seedRoleCascadeChildren(t, tn.ID, role.ID,
		[]string{"orbeat-vk-nightly", "orbeat-vk-ci"}, &quota)

	rec := roleDeleteRequest(t, srv, tok, role.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete role = %d, want 200, body %s", rec.Code, rec.Body)
	}

	evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("audit events = %d, want 1: %+v", len(evs), evs)
	}
	md := evs[0].Metadata

	if n, ok := md["virtualKeysRevoked"].(float64); !ok || n != 2 {
		t.Errorf("metadata[virtualKeysRevoked] = %v, want 2", md["virtualKeysRevoked"])
	}
	// Ordered by client_id, seeded out of that order on purpose.
	ids, ok := md["virtualKeyClientIds"].([]any)
	if !ok || len(ids) != 2 || ids[0] != "orbeat-vk-ci" || ids[1] != "orbeat-vk-nightly" {
		t.Errorf("metadata[virtualKeyClientIds] = %v, want [orbeat-vk-ci orbeat-vk-nightly]",
			md["virtualKeyClientIds"])
	}
	if n, ok := md["usageRowsDeleted"].(float64); !ok || n != 2 {
		t.Errorf("metadata[usageRowsDeleted] = %v, want 2", md["usageRowsDeleted"])
	}
	if n, ok := md["usageCallsDeleted"].(float64); !ok || n != 13 {
		t.Errorf("metadata[usageCallsDeleted] = %v, want 13 (9 + 4)", md["usageCallsDeleted"])
	}
	if n, ok := md["quotaMonthlyCalls"].(float64); !ok || n != 25000 {
		t.Errorf("metadata[quotaMonthlyCalls] = %v, want 25000", md["quotaMonthlyCalls"])
	}

	// The cascade really fired: a report nobody can verify is worth nothing.
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("verify pool: %v", err)
	}
	defer pool.Close()
	for _, table := range []string{"virtual_key", "role_quota", "usage_daily"} {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE tenant_id = $1 AND role_id = $2`,
			tn.ID, role.ID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d row(s) for the deleted role", table, n)
		}
	}
}

// TestAdminDeleteRoleAuditsAbsentQuotaAsNull pins the pointer's other state at
// the JSON boundary, which is where it is observable. "no quota row existed"
// and "a quota of zero was destroyed" are different facts to whoever
// re-creates the role, so the key is present and null rather than absent: an
// absent key would read, to anyone querying the audit table, exactly like an
// audit row written before this metadata existed.
func TestAdminDeleteRoleAuditsAbsentQuotaAsNull(t *testing.T) {
	ctx := context.Background()
	srv, st, tn, tok := newPagingServer(t)

	role, err := st.CreateRole(ctx, tn.ID, "no-quota-role")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	seedRoleCascadeChildren(t, tn.ID, role.ID, nil, nil)

	if rec := roleDeleteRequest(t, srv, tok, role.ID); rec.Code != http.StatusOK {
		t.Fatalf("delete role = %d, want 200, body %s", rec.Code, rec.Body)
	}
	evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("audit events = %d, want 1", len(evs))
	}
	md := evs[0].Metadata
	v, present := md["quotaMonthlyCalls"]
	if !present {
		t.Error("metadata has no quotaMonthlyCalls key at all; it must be present and null")
	}
	if v != nil {
		t.Errorf("metadata[quotaMonthlyCalls] = %v, want null", v)
	}
	if n, ok := md["virtualKeysRevoked"].(float64); !ok || n != 0 {
		t.Errorf("metadata[virtualKeysRevoked] = %v, want 0", md["virtualKeysRevoked"])
	}
	// Never JSON null, matching servers/artifacts: a caller reading this row
	// must not have to tell an empty list from a missing one.
	ids, ok := md["virtualKeyClientIds"].([]any)
	if !ok || len(ids) != 0 {
		t.Errorf("metadata[virtualKeyClientIds] = %v, want []", md["virtualKeyClientIds"])
	}
}
