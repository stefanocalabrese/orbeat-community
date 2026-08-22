package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/auth"
	"github.com/stefanocalabrese/orbeat-community/internal/authz"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
)

// TestMaxBytesMiddlewareCapsPostAndPutBodies proves maxBytesMiddleware ITSELF
// (called directly, not through Handler()) caps a POST/PUT body at
// maxRequestBodyBytes: reading past the cap surfaces *http.MaxBytesError to
// the downstream handler (which decodeJSONOrFail maps to 413). It says
// nothing about whether Handler() actually wires this middleware into its
// chain — that is TestMaxBytesMiddlewareWiredIntoHandlerCapsAdminPost below,
// which drives the real router end-to-end.
func TestMaxBytesMiddlewareCapsPostAndPutBodies(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			var gotErr error
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, gotErr = io.ReadAll(r.Body)
			})
			h := maxBytesMiddleware(next)

			big := bytes.Repeat([]byte("a"), maxRequestBodyBytes+1)
			req := httptest.NewRequest(method, "/x", bytes.NewReader(big))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			var maxErr *http.MaxBytesError
			if !errors.As(gotErr, &maxErr) {
				t.Fatalf("want *http.MaxBytesError reading an oversized %s body, got %v", method, gotErr)
			}
		})
	}
}

// TestMaxBytesMiddlewareLeavesGETBodyUnbounded proves the cap is scoped to
// mutating methods (per the audit's B3 fix description): a GET body larger
// than the cap still reads in full, unaffected.
func TestMaxBytesMiddlewareLeavesGETBodyUnbounded(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte(strconv.Itoa(len(b))))
	})
	h := maxBytesMiddleware(next)

	big := bytes.Repeat([]byte("a"), maxRequestBodyBytes+1)
	req := httptest.NewRequest(http.MethodGet, "/x", bytes.NewReader(big))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Body.String() != strconv.Itoa(len(big)) {
		t.Fatalf("GET body was capped: got %s bytes read, want %d", rec.Body.String(), len(big))
	}
}

// TestMaxBytesMiddlewareWiredIntoHandlerCapsAdminPost drives the REAL
// Server.Handler() chain (otelhttp -> logging.Requests -> corsMiddleware ->
// maxBytesMiddleware -> mux, api.go:262) end-to-end: a real RS256 admin
// token through RequireAuth -> RequireRole -> resolver.Middleware into
// handleCreateServer, whose decodeJSONOrFail maps *http.MaxBytesError to 413
// (admin_servers.go:85-96). Unlike TestMaxBytesMiddlewareCapsPostAndPutBodies
// above (which calls maxBytesMiddleware directly and proves nothing about
// Handler()'s wiring), this is the test in the package that fails if
// maxBytesMiddleware(mux) is ever dropped from that chain.
//
// The oversized body must be syntactically valid JSON up to the cap, or
// json.Decoder's scanner rejects it as a syntax error (400) on the first
// invalid byte, well before it ever reads past maxRequestBodyBytes — a body
// of repeated "a" characters (as the unit test above uses directly against
// io.ReadAll) is invalid JSON from byte 0 and would never exercise the
// reader cap through a real decode. A single oversized JSON string value
// keeps the decoder reading legitimate content past the cap.
func TestMaxBytesMiddlewareWiredIntoHandlerCapsAdminPost(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tenantName := fmt.Sprintf("maxbytes-%d", time.Now().UnixNano())
	idp := newMWOrderTestIdP(t)
	v, err := auth.NewValidator(ctx, auth.Config{Issuer: idp.srv.URL, Audience: "orbeat-api"})
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	srv := New(st, authz.NewResolver(st, tenantName), v, nil, nil)
	tok := idp.token(t, "kc-maxbytes-admin", []string{"orbeat-admin"})

	pad := strings.Repeat("a", maxRequestBodyBytes+1024)
	body := `{"name":"` + pad + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/servers", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized POST through the real Handler() = %d, want 413, body=%s", rec.Code, rec.Body)
	}
}
