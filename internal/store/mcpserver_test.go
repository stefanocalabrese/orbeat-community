package store

import (
	"context"
	"errors"
	"testing"
)

func TestMCPServerCreateListGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	in := MCPServer{
		TenantID:          tn.ID,
		Name:              "github",
		Description:       "GitHub MCP",
		Transport:         "http",
		EndpointOrCommand: "https://mcp.example/github",
		Version:           "1.0.0",
		ProtocolVersion:   "2025-06-18",
		SecretRef:         "vault:kv/github#token",
		Status:            "active",
	}
	created, err := s.CreateMCPServer(ctx, in)
	if err != nil {
		t.Fatalf("CreateMCPServer: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected non-empty ID")
	}

	got, err := s.GetMCPServer(ctx, tn.ID, created.ID)
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if got.Name != "github" || got.Transport != "http" || got.SecretRef != "vault:kv/github#token" {
		t.Fatalf("GetMCPServer mismatch: %+v", got)
	}

	list, err := s.ListMCPServersByTenant(ctx, tn.ID)
	if err != nil {
		t.Fatalf("ListMCPServersByTenant: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want exactly the created server", list)
	}
}

func TestMCPServerUpdateDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)
	srv, _ := s.CreateMCPServer(ctx, MCPServer{TenantID: tn.ID, Name: "gh", Transport: "http", EndpointOrCommand: "https://x", Status: "active"})

	srv.Description = "GitHub MCP"
	srv.Status = "disabled"
	upd, err := s.UpdateMCPServer(ctx, srv)
	if err != nil {
		t.Fatalf("UpdateMCPServer: %v", err)
	}
	if upd.Description != "GitHub MCP" || upd.Status != "disabled" {
		t.Fatalf("update mismatch: %+v", upd)
	}

	if err := s.DeleteMCPServer(ctx, tn.ID, srv.ID); err != nil {
		t.Fatalf("DeleteMCPServer: %v", err)
	}
	if _, err := s.GetMCPServer(ctx, tn.ID, srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMCPServer after delete: want ErrNotFound, got %v", err)
	}

	// Deleting the same server again must return ErrNotFound.
	if err := s.DeleteMCPServer(ctx, tn.ID, srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteMCPServer: want ErrNotFound on second delete, got %v", err)
	}
}

func TestDeleteMCPServerNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	const bogusID = "00000000-0000-0000-0000-000000000000"

	// GetMCPServer on a non-existent ID must return ErrNotFound.
	if _, err := s.GetMCPServer(ctx, tn.ID, bogusID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMCPServer(bogus): want ErrNotFound, got %v", err)
	}

	// Deleting a bogus UUID must return ErrNotFound.
	if err := s.DeleteMCPServer(ctx, tn.ID, bogusID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteMCPServer(bogus): want ErrNotFound, got %v", err)
	}
}

// TestGetMCPServerIsTenantScoped proves the STORE itself (not just an api
// layer's Go-level comparison) refuses to return a server for a tenant it
// does not belong to: GetMCPServer must filter by tenant_id in SQL.
func TestGetMCPServerIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tnA := mustTenant(t, s)
	tnB := mustTenant(t, s)

	srv, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tnA.ID, Name: "owned-by-a", Transport: "http",
		EndpointOrCommand: "https://x", Status: "active",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if got, err := s.GetMCPServer(ctx, tnA.ID, srv.ID); err != nil || got.ID != srv.ID {
		t.Fatalf("owning tenant get: %v %+v", err, got)
	}
	if _, err := s.GetMCPServer(ctx, tnB.ID, srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get: want ErrNotFound, got %v", err)
	}
}

// TestMCPServerMalformedIDIsNotFound proves a non-UUID id is treated as
// ErrNotFound (mapping Postgres 22P02 invalid_text_representation), not
// surfaced as a raw driver error that would 500 at the API layer. Covers
// every mcp_server store function reachable directly from a handler with a
// raw path-value id: handleGetServer and handleDeleteServer precede their
// store call with no separate fetch at all, and handleUpdateServer's own
// precondition read (a Task-3 stopgap — see admin_servers.go) is not a
// guarantee this test can rely on either, since it is slated for replacement
// in Task 6 — each store function must defend itself regardless of what its
// caller currently happens to do.
func TestMCPServerMalformedIDIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)
	const badID = "not-a-uuid"

	if _, err := s.GetMCPServer(ctx, tn.ID, badID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMCPServer(bad id): want ErrNotFound, got %v", err)
	}

	if _, err := s.UpdateMCPServer(ctx, MCPServer{
		ID: badID, TenantID: tn.ID, Name: "x", Transport: "http",
		EndpointOrCommand: "https://x", Status: "active",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateMCPServer(bad id): want ErrNotFound, got %v", err)
	}

	if err := s.DeleteMCPServer(ctx, tn.ID, badID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteMCPServer(bad id): want ErrNotFound, got %v", err)
	}

	if ok, err := s.MCPServerExistsInTenant(ctx, tn.ID, badID); err != nil || ok {
		t.Fatalf("MCPServerExistsInTenant(bad id): want (false, nil), got (%v, %v)", ok, err)
	}
}

func TestMCPServerRejectsBadStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	_, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID:          tn.ID,
		Name:              "bad-status",
		Transport:         "http",
		EndpointOrCommand: "https://x",
		Status:            "bogus",
	})
	if err == nil {
		t.Fatal("expected CHECK constraint to reject invalid status")
	}
}

// TestMCPServerTLSCARefRoundTrip proves the new column survives create, get and
// update, and that an unset ref reads back as "" rather than failing to scan.
// It mirrors how SecretRef is modelled: nullable in SQL, "" in Go.
func TestMCPServerTLSCARefRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	withRef, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "with-ca", Transport: "http",
		EndpointOrCommand: "https://internal.example.com/mcp",
		TLSCARef:          "env:INTERNAL_CA_PEM", Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if withRef.TLSCARef != "env:INTERNAL_CA_PEM" {
		t.Fatalf("create returned TLSCARef %q", withRef.TLSCARef)
	}
	got, err := s.GetMCPServer(ctx, tn.ID, withRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TLSCARef != "env:INTERNAL_CA_PEM" {
		t.Fatalf("get returned TLSCARef %q", got.TLSCARef)
	}

	// Unset must read back as "" (NULL in SQL), not fail to scan.
	plain, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "no-ca", Transport: "http",
		EndpointOrCommand: "https://public.example.com/mcp", Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain.TLSCARef != "" {
		t.Fatalf("unset TLSCARef read back as %q, want empty", plain.TLSCARef)
	}
}

// TestMCPServerTLSCARefUpdateSetAndClear proves UpdateMCPServer can both set
// the ref on a server that started without one, and clear it back to NULL —
// exercising the write-side NULLIF normalization in both directions.
func TestMCPServerTLSCARefUpdateSetAndClear(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tn := mustTenant(t, s)

	srv, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID: tn.ID, Name: "toggle-ca", Transport: "http",
		EndpointOrCommand: "https://x.example.com/mcp", Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if srv.TLSCARef != "" {
		t.Fatalf("newly created server has TLSCARef %q, want empty", srv.TLSCARef)
	}

	srv.TLSCARef = "env:OTHER_CA"
	updated, err := s.UpdateMCPServer(ctx, srv)
	if err != nil {
		t.Fatal(err)
	}
	if updated.TLSCARef != "env:OTHER_CA" {
		t.Fatalf("update set returned %q, want env:OTHER_CA", updated.TLSCARef)
	}

	updated.TLSCARef = ""
	cleared, err := s.UpdateMCPServer(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.TLSCARef != "" {
		t.Fatalf("update clear returned %q, want empty", cleared.TLSCARef)
	}

	// Raw-SQL assertion, and it is not redundant with the check above. Clearing
	// the ref must store NULL, not the empty string — but scanMCPServer collapses
	// both to "" (a NULL scans into *string as nil and is skipped; a '' scans as
	// a non-nil pointer to ""), so NO struct-level assertion can tell them apart.
	// Without this, removing NULLIF($n,'') from the UPDATE leaves the whole
	// package green: measured during Task 1's red-proof, where that mutant was
	// the one row that did not fail.
	var isNull bool
	if err := s.db.QueryRow(ctx,
		`SELECT tls_ca_ref IS NULL FROM mcp_server WHERE id=$1`, cleared.ID,
	).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatal("cleared tls_ca_ref stored as '' rather than NULL — NULLIF is missing from the UPDATE")
	}
}

func TestMCPServerRejectsBadTransport(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tn := mustTenant(t, s)

	_, err := s.CreateMCPServer(ctx, MCPServer{
		TenantID:          tn.ID,
		Name:              "bad",
		Transport:         "carrier-pigeon",
		EndpointOrCommand: "x",
		Status:            "active",
	})
	if err == nil {
		t.Fatal("expected CHECK constraint to reject invalid transport")
	}
}
