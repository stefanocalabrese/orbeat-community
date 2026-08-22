package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Tenant is an isolation boundary; single-tenant deploys have exactly one.
type Tenant struct {
	ID   string
	Name string
}

// CreateTenant inserts a tenant and returns it.
func (s *Store) CreateTenant(ctx context.Context, name string) (Tenant, error) {
	var t Tenant
	err := s.db.QueryRow(ctx,
		`INSERT INTO tenant (name) VALUES ($1) RETURNING id::text, name`,
		name,
	).Scan(&t.ID, &t.Name)
	if err != nil {
		return Tenant{}, fmt.Errorf("create tenant: %w", err)
	}
	return t, nil
}

// GetTenant fetches a tenant by id.
func (s *Store) GetTenant(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	err := s.db.QueryRow(ctx,
		`SELECT id::text, name FROM tenant WHERE id = $1`,
		id,
	).Scan(&t.ID, &t.Name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Tenant{}, ErrNotFound
		}
		return Tenant{}, fmt.Errorf("get tenant: %w", err)
	}
	return t, nil
}

// GetOrCreateTenantByName returns the tenant with the given name, creating it if
// absent. Relies on the UNIQUE(name) constraint for race-safe upsert.
//
// SELECT-first (audit B4): this runs on every authenticated request via
// authz.Resolver.Resolve, but a single-tenant deploy has exactly one tenant
// row — the pre-fix ON-CONFLICT-DO-UPDATE unconditionally rewrote that one row
// on every single request (new xmin, WAL record, dead tuple), so all
// concurrent requests briefly serialized on its row lock. After the first
// caller creates the row, every subsequent Resolve now takes a plain SELECT
// (no lock, no write) and only falls back to the upsert when the row is
// genuinely absent (first-ever boot, or a concurrent racing first-create,
// which the ON CONFLICT still resolves safely).
func (s *Store) GetOrCreateTenantByName(ctx context.Context, name string) (Tenant, error) {
	var t Tenant
	err := s.db.QueryRow(ctx,
		`SELECT id::text, name FROM tenant WHERE name = $1`,
		name,
	).Scan(&t.ID, &t.Name)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, fmt.Errorf("get-or-create tenant: select: %w", err)
	}
	err = s.db.QueryRow(ctx, `
		INSERT INTO tenant (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id::text, name`,
		name,
	).Scan(&t.ID, &t.Name)
	if err != nil {
		return Tenant{}, fmt.Errorf("get-or-create tenant: upsert: %w", err)
	}
	return t, nil
}
