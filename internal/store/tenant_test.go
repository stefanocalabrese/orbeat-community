package store

import (
	"context"
	"testing"
)

func TestGetOrCreateTenantByName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	name := "acme-" + t.Name()

	a, err := s.GetOrCreateTenantByName(ctx, name)
	if err != nil {
		t.Fatalf("first GetOrCreateTenantByName: %v", err)
	}
	if a.ID == "" || a.Name != name {
		t.Fatalf("unexpected tenant %+v", a)
	}
	b, err := s.GetOrCreateTenantByName(ctx, name)
	if err != nil {
		t.Fatalf("second GetOrCreateTenantByName: %v", err)
	}
	if b.ID != a.ID {
		t.Fatalf("expected stable ID on repeat, got %s then %s", a.ID, b.ID)
	}
}

// TestGetOrCreateTenantByNameSteadyStateNoWrite pins audit B4: once the
// single tenant row exists, a repeat resolve of the same name must not
// rewrite it. Postgres bumps a row's xmin (system column recording the
// inserting/updating transaction) on every UPDATE — including a no-op
// `SET name = EXCLUDED.name` — so xmin stability is a direct, cheap proxy for
// "no write happened." Pre-fix, this test fails: the unconditional
// ON-CONFLICT-DO-UPDATE rewrites the row on every single call.
func TestGetOrCreateTenantByNameSteadyStateNoWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	name := "acme-steady-" + t.Name()

	a, err := s.GetOrCreateTenantByName(ctx, name)
	if err != nil {
		t.Fatalf("first GetOrCreateTenantByName: %v", err)
	}
	xminBefore := tenantXmin(t, s, a.ID)

	b, err := s.GetOrCreateTenantByName(ctx, name)
	if err != nil {
		t.Fatalf("second GetOrCreateTenantByName: %v", err)
	}
	if b.ID != a.ID {
		t.Fatalf("expected stable ID on repeat, got %s then %s", a.ID, b.ID)
	}

	xminAfter := tenantXmin(t, s, a.ID)
	if xminAfter != xminBefore {
		t.Fatalf("steady-state GetOrCreateTenantByName rewrote the tenant row: xmin %s -> %s (expected a SELECT-only fast path, no write)", xminBefore, xminAfter)
	}
}

// tenantXmin reads the raw Postgres system column xmin for row id, as a
// change-detector: any UPDATE (even one writing identical values) advances it.
func tenantXmin(t *testing.T, s *Store, id string) string {
	t.Helper()
	var xmin string
	if err := s.db.QueryRow(context.Background(), `SELECT xmin::text FROM tenant WHERE id = $1`, id).Scan(&xmin); err != nil {
		t.Fatalf("query tenant xmin: %v", err)
	}
	return xmin
}

func TestCreateAndGetTenant(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created, err := s.CreateTenant(ctx, "acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if created.Name != "acme" {
		t.Fatalf("Name = %q, want acme", created.Name)
	}

	got, err := s.GetTenant(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if got != created {
		t.Fatalf("GetTenant = %+v, want %+v", got, created)
	}
}
