package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestRequestsLogsOneStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "json", "info")
	h := Requests(log, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestID(r.Context()) == "" {
			t.Errorf("request id not in context")
		}
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/thing", nil))

	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatalf("response missing X-Request-Id")
	}
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("log not JSON: %v (%q)", err, buf.String())
	}
	if m["msg"] != "http_request" || m["method"] != "GET" || m["path"] != "/v1/thing" {
		t.Fatalf("bad request-log fields: %+v", m)
	}
	if m["status"].(float64) != 418 {
		t.Fatalf("status not captured: %+v", m)
	}
	if _, ok := m["duration_ms"]; !ok {
		t.Fatalf("duration_ms missing: %+v", m)
	}
	if m["request_id"] == "" || m["request_id"] == nil {
		t.Fatalf("request_id missing: %+v", m)
	}
}

func TestRequestsHonorsInboundRequestID(t *testing.T) {
	var buf bytes.Buffer
	h := Requests(New(&buf, "json", "info"), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") != "abc-123" {
		t.Fatalf("inbound request id not honored: %q", rec.Header().Get("X-Request-Id"))
	}
	if !bytes.Contains(buf.Bytes(), []byte("abc-123")) {
		t.Fatalf("log line should carry inbound request id: %q", buf.String())
	}
}

func TestRequestsPreservesFlusher(t *testing.T) {
	h := Requests(New(&bytes.Buffer{}, "json", "info"), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("wrapped ResponseWriter must still expose http.Flusher for streaming")
		}
		f.Flush()
	}))
	rec := httptest.NewRecorder() // httptest.ResponseRecorder implements Flush → sets .Flushed
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if !rec.Flushed {
		t.Fatalf("Flush() should have forwarded to the underlying recorder")
	}
}

func TestIdentityFromContextEnrichesLog(t *testing.T) {
	var buf bytes.Buffer
	identity := func(context.Context) (string, string) { return "acme", "alice" }
	h := Requests(New(&buf, "json", "info"), identity)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	var m map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m)
	if m["tenant"] != "acme" || m["subject"] != "alice" {
		t.Fatalf("request log should carry tenant/subject from the identity func: %+v", m)
	}
}

// TestRequestsRejectsOversizedRequestID pins audit G12: an inbound
// X-Request-Id longer than 64 chars is unbounded client-controlled data that
// would otherwise flow verbatim into logs/audit — it must be replaced with a
// freshly generated id, not honored.
func TestRequestsRejectsOversizedRequestID(t *testing.T) {
	var buf bytes.Buffer
	h := Requests(New(&buf, "json", "info"), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", strings.Repeat("a", 65))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := rec.Header().Get("X-Request-Id")
	if got == strings.Repeat("a", 65) {
		t.Fatalf("oversized request id was honored verbatim, want a generated replacement")
	}
	if got == "" {
		t.Fatal("expected a generated request id, got empty")
	}
}

// TestRequestsRejectsRequestIDWithBadChars pins the charset guard: a header
// containing characters outside [A-Za-z0-9_-] (e.g. CRLF/control chars used
// for log/header injection, or arbitrary punctuation) must be replaced.
func TestRequestsRejectsRequestIDWithBadChars(t *testing.T) {
	for _, bad := range []string{"abc def", "abc/def", "abc\r\ndef", "abc\ndef", "<script>"} {
		t.Run(bad, func(t *testing.T) {
			h := Requests(New(&bytes.Buffer{}, "json", "info"), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("X-Request-Id", bad)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Header().Get("X-Request-Id") == bad {
				t.Fatalf("invalid-charset request id %q was honored verbatim", bad)
			}
		})
	}
}

// TestRequestsHonorsValidRequestIDAtMaxLength pins the boundary: a
// well-formed 64-char id (the max allowed) must still be honored, not
// needlessly replaced.
func TestRequestsHonorsValidRequestIDAtMaxLength(t *testing.T) {
	valid := strings.Repeat("a", 64)
	h := Requests(New(&bytes.Buffer{}, "json", "info"), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", valid)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") != valid {
		t.Fatalf("valid 64-char request id was not honored: got %q", rec.Header().Get("X-Request-Id"))
	}
}

func TestRequestsAddsTraceIDWhenSpanActive(t *testing.T) {
	var buf bytes.Buffer
	tp := sdktrace.NewTracerProvider()
	tr := tp.Tracer("test")

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := Requests(New(&buf, "json", "info"), nil)(inner)

	ctx, span := tr.Start(context.Background(), "s")
	defer span.End()
	req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	var m map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m)
	if m["trace_id"] == nil || m["trace_id"] == "" {
		t.Fatalf("http_request line should carry trace_id when a span is active: %+v", m)
	}
}
