package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// TestServeOpenAPISpec proves GET /openapi.yaml is wired into Server.Handler()
// (beside GET /healthz) and served unauthenticated: no bearer token, no
// validator even configured, and the response is the embedded YAML doc.
func TestServeOpenAPISpec(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tenantName := fmt.Sprintf("openapi-serve-%d", time.Now().UnixNano())
	// validator nil: the route must not require auth at all, so Handler()
	// must never invoke it for this path.
	srv := New(st, authz.NewResolver(st, tenantName), nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	body := rec.Body.String()
	if len(body) == 0 || !strings.HasPrefix(body, "openapi:") {
		t.Errorf("body should be the embedded YAML starting with 'openapi:'; got %d bytes starting %q", len(body), body[:min(20, len(body))])
	}
}
