package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewJSONEmitsStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "json", "info")
	log.Info("hello", "k", "v")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, buf.String())
	}
	if m["msg"] != "hello" || m["k"] != "v" || m["level"] != "INFO" {
		t.Fatalf("bad fields: %+v", m)
	}
}

func TestNewLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "json", "warn")
	log.Info("suppressed")
	log.Warn("shown")
	if strings.Contains(buf.String(), "suppressed") {
		t.Fatalf("info line should be filtered at warn level: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "shown") {
		t.Fatalf("warn line should appear: %q", buf.String())
	}
}

func TestNewTextFormat(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "text", "info")
	log.Info("hi")
	if !strings.Contains(buf.String(), "hi") {
		t.Fatalf("text output missing msg: %q", buf.String())
	}
	if json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Fatalf("text format should not be valid JSON: %q", buf.String())
	}
}

func TestNewUnknownFormatAndLevelFallSafe(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "yaml", "chatty") // both invalid → json + info
	log.Info("x")
	if !json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Fatalf("unknown format must fall back to json: %q", buf.String())
	}
}
