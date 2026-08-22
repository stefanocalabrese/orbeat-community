package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/migrate"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

var testDSN string

func TestMain(m *testing.M) {
	ctx := context.Background()
	pg, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("orbeat"), tcpostgres.WithUsername("orbeat"), tcpostgres.WithPassword("orbeat"),
		testcontainers.WithName("orbeat-api-tests"),
		testcontainers.WithWaitStrategy(wait.ForAll(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second))))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dsn, _ := pg.ConnectionString(ctx, "sslmode=disable")
	db, _ := sql.Open("pgx", dsn)
	if err := migrate.Up(db); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = db.Close()
	testDSN = dsn
	code := m.Run()
	_ = pg.Terminate(ctx)
	os.Exit(code)
}

func injectPrincipal(ctx context.Context) context.Context {
	return auth.WithPrincipal(ctx, auth.Principal{Subject: "kc-1", Email: "a@x.io", Roles: []string{"orbeat-user"}})
}

func TestCatalogFiltersByEntitlement(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	tn, _ := s.GetOrCreateTenantByName(ctx, "default")
	role, _ := s.CreateRole(ctx, tn.ID, "orbeat-user")
	entitled, _ := s.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "github", Transport: "http", EndpointOrCommand: "https://x", SecretRef: "vault:kv/gh#token", Status: "active"})
	_, _ = s.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "secret-svc", Transport: "http", EndpointOrCommand: "https://y", Status: "active"})
	_, _ = s.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: entitled.ID, Permissions: []string{}})

	srv := New(s, authz.NewResolver(s, "default"), nil, nil, nil) // validator nil: we inject ResolvedContext directly

	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1", RoleIDs: []string{role.ID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleCatalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Servers) != 1 || body.Servers[0]["name"] != "github" {
		t.Fatalf("expected only entitled 'github', got %+v", body.Servers)
	}
	// SECURITY: secret_ref must never be exposed.
	if _, leaked := body.Servers[0]["secret_ref"]; leaked {
		t.Fatal("catalog leaked secret_ref")
	}
	if _, leaked := body.Servers[0]["secretRef"]; leaked {
		t.Fatal("catalog leaked secretRef")
	}
	// allowedTools: this entitlement has nil AllowedTools → all tools → JSON null.
	if v, ok := body.Servers[0]["allowedTools"]; !ok || v != nil {
		t.Fatalf("allowedTools = %v (present=%v), want JSON null", v, ok)
	}
}

func TestCatalogHidesNonActiveServers(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)

	tn, _ := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("cat-status-%d", time.Now().UnixNano()))
	role, _ := s.CreateRole(ctx, tn.ID, "orbeat-user")
	// Two entitled servers: one live, one retired. Only "active" servers are exposed.
	active, _ := s.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "live", Transport: "http", EndpointOrCommand: "https://x", Status: "active"})
	disabled, _ := s.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "retired", Transport: "http", EndpointOrCommand: "https://y", Status: "disabled"})
	_, _ = s.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: active.ID})
	_, _ = s.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: disabled.ID})

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1", RoleIDs: []string{role.ID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleCatalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := map[string]bool{}
	for _, sv := range body.Servers {
		names[sv["name"].(string)] = true
	}
	// Selective: the active entitled server is present...
	if !names["live"] {
		t.Fatalf("active server 'live' must be present, got %+v", body.Servers)
	}
	// ...while the non-active (but entitled) server is hidden.
	if names["retired"] {
		t.Fatalf("non-active server 'retired' must be hidden from the catalog, got %+v", body.Servers)
	}
	if len(body.Servers) != 1 {
		t.Fatalf("expected only the active server, got %d: %+v", len(body.Servers), body.Servers)
	}
}

func TestCatalogExposesCallersAllowedTools(t *testing.T) {
	ctx := context.Background()
	s, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(s.Close)
	tn, _ := s.GetOrCreateTenantByName(ctx, fmt.Sprintf("cat-tools-%d", time.Now().UnixNano()))
	role, _ := s.CreateRole(ctx, tn.ID, "orbeat-user")
	restricted, _ := s.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "restricted", Transport: "http", EndpointOrCommand: "https://x", Status: "active"})
	openSrv, _ := s.CreateMCPServer(ctx, store.MCPServer{TenantID: tn.ID, Name: "open", Transport: "http", EndpointOrCommand: "https://y", Status: "active"})
	_, _ = s.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: restricted.ID, AllowedTools: []string{"echo"}})
	_, _ = s.CreateEntitlement(ctx, store.Entitlement{TenantID: tn.ID, RoleID: role.ID, MCPServerID: openSrv.ID}) // nil = all tools

	srv := New(s, authz.NewResolver(s, tn.Name), nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	rc := authz.ResolvedContext{TenantID: tn.ID, UserID: "u1", RoleIDs: []string{role.ID}}
	req = req.WithContext(authz.WithResolved(injectPrincipal(req.Context()), rc))
	rec := httptest.NewRecorder()
	srv.handleCatalog(rec, req)

	var body struct {
		Servers []struct {
			Name         string    `json:"name"`
			AllowedTools *[]string `json:"allowedTools"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]*[]string{}
	for _, sv := range body.Servers {
		byName[sv.Name] = sv.AllowedTools
	}
	if byName["open"] != nil && *byName["open"] != nil {
		t.Fatalf("open should have null allowedTools, got %v", *byName["open"])
	}
	if byName["restricted"] == nil || len(*byName["restricted"]) != 1 || (*byName["restricted"])[0] != "echo" {
		t.Fatalf("restricted allowedTools = %v, want [echo]", byName["restricted"])
	}
}
