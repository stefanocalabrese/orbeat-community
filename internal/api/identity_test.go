package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/ratelimit"
)

// TestRequestLogCarriesIdentity pins the defect that shipped: apiIdentity read
// a ctx snapshot taken BEFORE resolver.Middleware ran, so it returned ("", "")
// on every request and the request log never carried identity, despite its doc
// comment saying it did. Measured before the fix: 85 status=200 lines across
// this suite, zero with a subject field.
//
// It drives the REAL handler chain and inspects the REAL emitted log line,
// never the wiring source, because a source-parsing assertion cannot be
// red-proven: go test -overlay substitutes what the compiler sees, not what a
// test reads from disk at runtime.
//
// Red-proof: drop withIdentityCarrier from Handler(), or move recordIdentity
// outside resolver.Middleware. Either leaves the log line without the field.
func TestRequestLogCarriesIdentity(t *testing.T) {
	var logBuf bytes.Buffer
	// rps 0 disables limiting: these tests are about the log line, not throttling.
	srv, _, idp, _ := newRateLimitedServer(t, "identity-log", ratelimit.New(0, 1, time.Minute, 10))
	srv.logger = slog.New(slog.NewJSONHandler(&logBuf, nil))

	tok := idp.token(t, "kc-identity-log", []string{"orbeat-admin"})
	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/catalog = %d, want 200, body=%s", rec.Code, rec.Body)
	}

	// Assert the VALUES, not the key names. logging.Requests appends BOTH keys
	// whenever EITHER value is non-empty, so `"tenant"` is present as an empty
	// string even when tenant was never resolved. A Contains check on the key
	// therefore passes on a mutant that moves recordIdentity outside
	// resolver.Middleware, which is exactly what it must catch. Proven: that
	// mutant was GREEN against the key-presence version of this test.
	var got struct {
		Msg     string `json:"msg"`
		Tenant  string `json:"tenant"`
		Subject string `json:"subject"`
	}
	line := strings.TrimSpace(logBuf.String())
	if line == "" {
		t.Fatal("no log output at all")
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("log line is not JSON: %v; got: %s", err, line)
	}
	if got.Msg != "http_request" {
		t.Fatalf("log msg = %q, want http_request; got: %s", got.Msg, line)
	}
	if got.Subject == "" {
		t.Errorf("http_request carries an EMPTY subject (the shipped defect); got: %s", line)
	}
	if got.Tenant == "" {
		t.Errorf("http_request carries an EMPTY tenant, so identity was recorded outside the resolver; got: %s", line)
	}
}

// TestRequestLogIdentityIsAbsentNotFabricated is the other half. An
// unauthenticated request resolves no identity, and the correct behaviour is to
// omit the fields rather than emit empty or invented ones. Without this, a fix
// that hardcoded a value would satisfy the test above.
func TestRequestLogIdentityIsAbsentNotFabricated(t *testing.T) {
	var logBuf bytes.Buffer
	srv, _, _, _ := newRateLimitedServer(t, "identity-anon", ratelimit.New(0, 1, time.Minute, 10))
	srv.logger = slog.New(slog.NewJSONHandler(&logBuf, nil))

	req := httptest.NewRequest(http.MethodGet, "/v1/catalog", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /v1/catalog = %d, want 401", rec.Code)
	}
	line := logBuf.String()
	if !strings.Contains(line, `"msg":"http_request"`) {
		t.Fatalf("401 was not logged; the log line must survive auth failure: %s", line)
	}
	if strings.Contains(line, `"subject"`) || strings.Contains(line, `"tenant"`) {
		t.Errorf("401 log line carries identity fields it cannot know; got: %s", line)
	}
}
