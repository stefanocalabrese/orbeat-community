package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/version"
)

// TestRenderedOpenAPISpecSubstitutesVersion proves renderedOpenAPISpec
// replaces the embedded placeholder with the given version, and leaves the
// rest of the document untouched.
func TestRenderedOpenAPISpecSubstitutesVersion(t *testing.T) {
	got := renderedOpenAPISpec("9.9.9-test")
	if !bytes.Contains(got, []byte("version: 9.9.9-test")) {
		t.Fatalf("renderedOpenAPISpec output does not contain the substituted version; got:\n%s", got[:min(300, len(got))])
	}
	if bytes.Contains(got, []byte(openapiVersionPlaceholder)) {
		t.Fatalf("renderedOpenAPISpec output still contains the placeholder %q — substitution did not happen", openapiVersionPlaceholder)
	}
	// Nothing else in the document should move: same length delta as the
	// substituted text, and the tag list right after info: still present.
	if !bytes.Contains(got, []byte("servers:")) {
		t.Fatal("renderedOpenAPISpec corrupted the document — the servers: section is gone")
	}
}

// TestRenderedOpenAPISpecRequiresPlaceholder proves the panic guard is
// reachable: if openapi.yaml is ever edited so it no longer contains the
// exact placeholder line, serving would otherwise silently ship an
// unsubstituted (or garbage) version forever. This temporarily substitutes
// the package-level embedded var, not a copy on disk — the embed content
// itself is exercised by TestOpenAPIVersionPlaceholderPresentInEmbeddedFile
// below.
func TestRenderedOpenAPISpecRequiresPlaceholder(t *testing.T) {
	orig := openapiSpec
	t.Cleanup(func() { openapiSpec = orig })
	openapiSpec = []byte("openapi: 3.0.3\ninfo:\n  version: 1.2.3\n")

	defer func() {
		if recover() == nil {
			t.Fatal("renderedOpenAPISpec did not panic on a document missing the version placeholder")
		}
	}()
	renderedOpenAPISpec("1.2.3")
}

// TestOpenAPIVersionPlaceholderPresentInEmbeddedFile pins that the REAL
// embedded openapi.yaml (not a substitute) still carries the exact
// placeholder renderedOpenAPISpec looks for — catching an edit to
// openapi.yaml's info.version line that doesn't also update the marker
// constant, which would make every served document panic in production.
func TestOpenAPIVersionPlaceholderPresentInEmbeddedFile(t *testing.T) {
	if !bytes.Contains(openapiSpec, []byte(openapiVersionPlaceholder)) {
		t.Fatalf("the embedded openapi.yaml no longer contains %q — renderedOpenAPISpec would panic on every request", openapiVersionPlaceholder)
	}
}

// TestCurrentOpenAPISpecTracksVersion proves currentOpenAPISpec reads
// internal/version.Version live on every call, not a value captured once —
// the same live-read requirement as internal/gateway's gatewayImplementation.
func TestCurrentOpenAPISpecTracksVersion(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })

	for _, v := range []string{"dev", "openapitest-4b2e"} {
		version.Version = v
		got := currentOpenAPISpec()
		want := []byte("version: " + v)
		if !bytes.Contains(got, want) {
			t.Fatalf("currentOpenAPISpec() with version.Version=%q does not contain %q", v, want)
		}
	}
}

// TestServeOpenAPISpecUsesLiveVersion drives the full HTTP path (not just the
// helper functions above) and proves GET /openapi.yaml serves whatever
// internal/version.Version currently holds, not the embedded placeholder.
func TestServeOpenAPISpecUsesLiveVersion(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })
	version.Version = "servetest-1a2b"

	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tenantName := fmt.Sprintf("openapi-version-%d", time.Now().UnixNano())
	srv := New(st, authz.NewResolver(st, tenantName), nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "version: servetest-1a2b") {
		t.Fatalf("served openapi.yaml does not contain the live version; want \"version: servetest-1a2b\", got first 300 bytes:\n%s", body[:min(300, len(body))])
	}
	if strings.Contains(body, openapiVersionPlaceholder) {
		t.Fatalf("served openapi.yaml still contains the unsubstituted placeholder %q", openapiVersionPlaceholder)
	}
}
