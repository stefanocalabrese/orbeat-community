// Package logging builds orbeat's structured JSON logger and HTTP request
// logging. Logs go to stdout as JSON (or text in dev); a platform shipper
// streams them onward — orbeat never writes to Kafka/CloudWatch directly.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// New builds a slog.Logger writing to w. format is "json" (default) or "text";
// level is debug|info|warn|error (default info). Unknown values fall back safely.
func New(w io.Writer, format, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
