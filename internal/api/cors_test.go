package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

func corsEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
}

func TestCORSAllowsConfiguredOrigin(t *testing.T) {
	h := corsMiddleware([]string{"http://localhost:8081"})(corsEcho())
	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	req.Header.Set("Origin", "http://localhost:8081")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8081" {
		t.Fatalf("ACAO = %q", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin", got)
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("handler not reached: %d", rec.Code)
	}
}

func TestCORSDisallowedOriginGetsNoHeaders(t *testing.T) {
	h := corsMiddleware([]string{"http://localhost:8081"})(corsEcho())
	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("disallowed origin must get no ACAO header")
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("disallowed origin still needs Vary: Origin, got %q", got)
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("request itself still passes through: %d", rec.Code)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := corsMiddleware([]string{"http://localhost:8081"})(corsEcho())
	req := httptest.NewRequest(http.MethodOptions, "/v1/admin/servers", nil)
	req.Header.Set("Origin", "http://localhost:8081")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" ||
		rec.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Fatal("preflight missing allow headers")
	}
}

// TestCORSPreflightAllowsRequestIDHeader proves the preflight advertises
// X-Request-Id in Access-Control-Allow-Headers, so a browser client may send the
// request-id the logging middleware honors (audit B6: latent drift otherwise).
func TestCORSPreflightAllowsRequestIDHeader(t *testing.T) {
	h := corsMiddleware([]string{"http://localhost:8081"})(corsEcho())
	req := httptest.NewRequest(http.MethodOptions, "/v1/admin/servers", nil)
	req.Header.Set("Origin", "http://localhost:8081")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "X-Request-Id") {
		t.Fatalf("preflight allow-headers must include X-Request-Id, got %q", got)
	}
}

func TestCORSNoConfigIsNoOp(t *testing.T) {
	h := corsMiddleware(nil)(corsEcho())
	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	req.Header.Set("Origin", "http://localhost:8081")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no-config must emit no CORS headers (fail closed)")
	}
}

// TestCORSAllowsIfMatchAndExposesETag guards the header plumbing the
// optimistic-concurrency feature rides on, and — since a review of this test
// found the identical bug already shipped on a second header — the audit
// export truncation warning too.
//
// Without If-Match in Access-Control-Allow-Headers the cross-origin PREFLIGHT
// fails and the browser never sends the PUT at all. Without ETag in
// Access-Control-Expose-Headers, res.headers.get("ETag") is null in JS.
// Without X-Orbeat-Export-Truncated in the same list,
// res.headers.get("X-Orbeat-Export-Truncated") (AuditPage.tsx) is equally
// null — the export-truncation warning has never fired in dev or CI since
// v1.8.0, for the same reason and by the same fix.
//
// This is invisible to the portal's unit tests (they mock fetch, which enforces
// no CORS) and invisible in production (docker-compose.prod.yml serves the SPA
// and API same-origin). It breaks DEV and CI only — the inverse of the usual
// prod-only-gap assumption, which is why it needs its own test.
func TestCORSAllowsIfMatchAndExposesETag(t *testing.T) {
	h := corsMiddleware([]string{"http://localhost:8081"})(corsEcho())

	t.Run("preflight allows If-Match", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/v1/admin/artifacts/x", nil)
		req.Header.Set("Origin", "http://localhost:8081")
		req.Header.Set("Access-Control-Request-Method", "PUT")
		req.Header.Set("Access-Control-Request-Headers", "If-Match")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "If-Match") {
			t.Fatalf("preflight allow-headers must include If-Match, got %q", got)
		}
	})

	t.Run("normal response exposes ETag", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/artifacts/x", nil)
		req.Header.Set("Origin", "http://localhost:8081")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "ETag") {
			t.Fatalf("response must expose ETag, got Access-Control-Expose-Headers = %q", got)
		}
	})

	t.Run("normal response exposes X-Orbeat-Export-Truncated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/export", nil)
		req.Header.Set("Origin", "http://localhost:8081")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "X-Orbeat-Export-Truncated") {
			t.Fatalf("response must expose X-Orbeat-Export-Truncated, got Access-Control-Expose-Headers = %q", got)
		}
	})

	t.Run("disallowed origin gets neither header", func(t *testing.T) {
		preflight := httptest.NewRequest(http.MethodOptions, "/v1/admin/artifacts/x", nil)
		preflight.Header.Set("Origin", "http://evil.example")
		preflight.Header.Set("Access-Control-Request-Method", "PUT")
		preflight.Header.Set("Access-Control-Request-Headers", "If-Match")
		precRec := httptest.NewRecorder()
		h.ServeHTTP(precRec, preflight)
		if got := precRec.Header().Get("Access-Control-Allow-Headers"); got != "" {
			t.Fatalf("disallowed origin must get no Access-Control-Allow-Headers, got %q", got)
		}

		normal := httptest.NewRequest(http.MethodGet, "/v1/admin/artifacts/x", nil)
		normal.Header.Set("Origin", "http://evil.example")
		normRec := httptest.NewRecorder()
		h.ServeHTTP(normRec, normal)
		if got := normRec.Header().Get("Access-Control-Expose-Headers"); got != "" {
			t.Fatalf("disallowed origin must get no Access-Control-Expose-Headers, got %q", got)
		}
	})
}

// TestCORSExposesETagOnRealAdminArtifactRoutes is the integration-style
// counterpart to TestCORSAllowsIfMatchAndExposesETag above: that test proves
// the header logic in isolation (corsMiddleware wrapping a bare echo
// handler), but corsMiddleware sits inside a specific wrapping order in
// Server.Handler() (otelhttp -> logging.Requests -> corsMiddleware ->
// maxBytesMiddleware -> mux). This test drives the REAL router — real
// RS256-token auth, real admin-role gate, real mux dispatch — for both a GET
// and a PUT to /v1/admin/artifacts/{id}, the two methods the
// optimistic-concurrency feature actually uses, and checks
// Access-Control-Expose-Headers on the response that reaches the client in
// each case. Because corsMiddleware wraps the mux from OUTSIDE routing, this
// also stands as evidence for every other route, not just these two.
func TestCORSExposesETagOnRealAdminArtifactRoutes(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tenantName := fmt.Sprintf("cors-etag-%d", time.Now().UnixNano())
	idp := newMWOrderTestIdP(t)
	v, err := auth.NewValidator(ctx, auth.Config{Issuer: idp.srv.URL, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}

	srv := New(st, authz.NewResolver(st, tenantName), v, []string{"http://localhost:8081"}, nil)
	tok := idp.token(t, "kc-cors-admin", []string{"orbeat-admin"})

	// The artifact need not exist: the CORS wrapper runs before the mux ever
	// dispatches to the handler, so the header must appear on the response
	// regardless of what the handler underneath ultimately returns (GET 404s
	// on the missing id; PUT 400s first, on the empty `{}` body, before it
	// ever gets to an id lookup — both are expected and irrelevant to what
	// this test checks).
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			var body *strings.Reader
			if method == http.MethodPut {
				body = strings.NewReader(`{}`)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(method, "/v1/admin/artifacts/does-not-exist", body)
			req.Header.Set("Authorization", "Bearer "+tok)
			req.Header.Set("Origin", "http://localhost:8081")
			if method == http.MethodPut {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "ETag") {
				t.Fatalf("%s response: Access-Control-Expose-Headers = %q, want it to contain ETag (status=%d, body=%s)",
					method, got, rec.Code, rec.Body)
			}
		})
	}
}

// TestCORSExposesAuditTruncationHeaderOnRealAuditExportRoute moved to
// admin_audit_export.ee_test.go: GET /v1/admin/audit/export is
// Enterprise-only, not registered by a generated Community tree's mux
// (docs/specs/2026-08-19-orbeat-community-repo-generation-design.md §4).
