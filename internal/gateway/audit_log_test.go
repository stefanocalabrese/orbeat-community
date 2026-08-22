package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/stefanocalabrese/orbeat-community/internal/logging"
	"github.com/stefanocalabrese/orbeat-community/internal/store"
	"github.com/stefanocalabrese/orbeat-community/internal/telemetry"
)

// TestAuditDualEmitsLogLine verifies that Server.audit, in addition to its
// best-effort DB write, always emits a structured "event":"audit" JSON log
// line (the SIEM-facing side of the dual-emit) — mirroring the api package's
// Task 4 audit-emit coverage for the gateway's own audit chokepoint.
func TestAuditDualEmitsLogLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, gwDSN)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	tn, err := st.GetOrCreateTenantByName(ctx, "audit-log-test")
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	var buf bytes.Buffer
	srv := &Server{store: st, logger: logging.New(&buf, "json", "info"), metrics: telemetry.NewMetrics(otel.Meter("gateway-test"))}
	sess := &session{tenantID: tn.ID, actor: "kc-alice"}

	srv.audit(ctx, sess, "gateway.tool.call", "fixture__echo", "allow")

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("audit: expected a log line, got none")
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("audit log line not valid JSON: %v\nline: %s", err, line)
	}
	if fields["event"] != "audit" {
		t.Fatalf("event = %v, want %q", fields["event"], "audit")
	}
	if fields["decision"] != "allow" {
		t.Fatalf("decision = %v, want %q", fields["decision"], "allow")
	}
	if fields["actor"] != "kc-alice" {
		t.Fatalf("actor = %v, want %q", fields["actor"], "kc-alice")
	}
	if fields["action"] != "gateway.tool.call" {
		t.Fatalf("action = %v, want %q", fields["action"], "gateway.tool.call")
	}
	if fields["tenant"] != tn.ID {
		t.Fatalf("tenant = %v, want %q", fields["tenant"], tn.ID)
	}

	evs, err := st.ListAuditEventsByTenant(ctx, tn.ID, 10)
	if err != nil {
		t.Fatalf("list audit events: %v", err)
	}
	if len(evs) != 1 || evs[0].Decision != "allow" {
		t.Fatalf("audit db row missing/wrong: %+v", evs)
	}
}
