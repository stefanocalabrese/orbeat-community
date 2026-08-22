package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// validRequestID bounds an inbound X-Request-Id to a safe, small charset
// before it is echoed back, logged, and (in the gateway) persisted into audit
// metadata: an unbounded/arbitrary client-controlled string is otherwise
// attacker-controlled data flowing straight into logs/audit with no size or
// content guard. 1-64 chars, [A-Za-z0-9_-] only — generous for UUIDs/ULIDs/
// trace ids while rejecting header-injection or bloat attempts.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type ctxKey int

const (
	loggerKey ctxKey = iota
	requestIDKey
)

// WithLogger returns ctx carrying a request-scoped logger.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// LoggerFrom returns the request-scoped logger, or slog.Default() if none.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// RequestID returns the request id from ctx, or "" if none.
func RequestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// newRequestID returns a random 128-bit hex id (crypto/rand — never math/rand).
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped writer's Flusher when present, so streaming
// responses (SSE / MCP Streamable HTTP) still flush through this wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer for
// capabilities not surfaced here (write deadlines, hijack, etc.).
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Requests logs one structured line per HTTP request and injects a request id +
// a request-scoped logger into the context. Honors an inbound X-Request-Id.
// identity, if non-nil, enriches each line with the resolved tenant/subject
// (passed in by the caller to keep logging decoupled from authz).
func Requests(base *slog.Logger, identity func(context.Context) (tenant, subject string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if !validRequestID.MatchString(id) {
				id = newRequestID()
			}
			w.Header().Set("X-Request-Id", id)

			reqLog := base.With("request_id", id)
			ctx := WithLogger(context.WithValue(r.Context(), requestIDKey, id), reqLog)

			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(sr, r.WithContext(ctx))

			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", sr.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", id,
			}
			if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
				attrs = append(attrs, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
			}
			if identity != nil {
				if tenant, subject := identity(ctx); tenant != "" || subject != "" {
					attrs = append(attrs, "tenant", tenant, "subject", subject)
				}
			}
			base.Info("http_request", attrs...)
		})
	}
}
