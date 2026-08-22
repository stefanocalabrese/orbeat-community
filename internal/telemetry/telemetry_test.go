package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// withCapturedDefaultLogger swaps slog's default logger for one writing to a
// buffer for the duration of fn, then restores the original. Setup emits its
// warnings via the package-level slog.Warn (no injectable logger), so this is
// the seam available to observe them.
func withCapturedDefaultLogger(t *testing.T, fn func(buf *bytes.Buffer)) {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	fn(&buf)
}

func boundedShutdown(t *testing.T, shutdown func(context.Context) error) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})
}

func TestParseSampleRatio(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want float64
		ok   bool
	}{
		{"1.0", 1.0, true},
		{"0.5", 0.5, true},
		{"0", 0, true},
		{"", 1.0, false},    // empty
		{"abc", 1.0, false}, // garbage
		{"-1", 1.0, false},  // out of range
		{"1.5", 1.0, false}, // out of range
	} {
		ratio, ok := Config{SampleRatio: tc.raw}.ParseSampleRatio()
		if ratio != tc.want || ok != tc.ok {
			t.Errorf("%q: got (%v, %v), want (%v, %v)", tc.raw, ratio, ok, tc.want, tc.ok)
		}
	}
}

func TestParseInsecure(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
		ok   bool
	}{
		{"", true, true}, // default: insecure, for back-compat
		{"true", true, true},
		{"TRUE", true, true},
		{"false", false, true},
		{"False", false, true},
		{"garbage", true, false}, // malformed → fall back to insecure, flagged
	} {
		insecure, ok := Config{Insecure: tc.raw}.ParseInsecure()
		if insecure != tc.want || ok != tc.ok {
			t.Errorf("%q: got (%v, %v), want (%v, %v)", tc.raw, insecure, ok, tc.want, tc.ok)
		}
	}
}

// TestSetupWarnsOnInsecureOTLPByDefault pins that leaving ORBEAT_OTEL_INSECURE
// unset (the back-compat default) still surfaces a startup Warn once telemetry
// is actually enabled — silence here would let an insecure production
// collector go unnoticed.
func TestSetupWarnsOnInsecureOTLPByDefault(t *testing.T) {
	withCapturedDefaultLogger(t, func(buf *bytes.Buffer) {
		_, shutdown, err := Setup(context.Background(), Config{Endpoint: "localhost:4317", ServiceName: "orbeat-test"})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		boundedShutdown(t, shutdown)
		if !strings.Contains(strings.ToLower(buf.String()), "insecure") {
			t.Errorf("expected an insecure-OTLP startup warning, got: %q", buf.String())
		}
	})
}

// TestSetupNoInsecureWarnWhenTLSEnabled pins that ORBEAT_OTEL_INSECURE=false
// suppresses the insecure-transport warning (it no longer applies) and uses
// TLS credentials instead of WithInsecure().
func TestSetupNoInsecureWarnWhenTLSEnabled(t *testing.T) {
	withCapturedDefaultLogger(t, func(buf *bytes.Buffer) {
		_, shutdown, err := Setup(context.Background(), Config{Endpoint: "localhost:4317", ServiceName: "orbeat-test", Insecure: "false"})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		boundedShutdown(t, shutdown)
		if strings.Contains(strings.ToLower(buf.String()), "without tls") {
			t.Errorf("must not warn about an insecure transport when TLS is enabled: %q", buf.String())
		}
	})
}

// TestSetupWarnsOnMalformedInsecureValue pins the warn-on-fallback contract
// for a garbage ORBEAT_OTEL_INSECURE value (mirrors ParseSampleRatio's).
func TestSetupWarnsOnMalformedInsecureValue(t *testing.T) {
	withCapturedDefaultLogger(t, func(buf *bytes.Buffer) {
		_, shutdown, err := Setup(context.Background(), Config{Endpoint: "localhost:4317", ServiceName: "orbeat-test", Insecure: "not-a-bool"})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		boundedShutdown(t, shutdown)
		if !strings.Contains(buf.String(), "ORBEAT_OTEL_INSECURE") {
			t.Errorf("expected a malformed ORBEAT_OTEL_INSECURE warning, got: %q", buf.String())
		}
	})
}

// TestSetupWarnsOnSampleRatioFallback pins the warn-on-fallback contract for a
// malformed ORBEAT_OTEL_SAMPLE_RATIO, mirroring
// Config.MarketplaceGitTimeoutDuration's (value, ok) pattern in internal/config.
func TestSetupWarnsOnSampleRatioFallback(t *testing.T) {
	withCapturedDefaultLogger(t, func(buf *bytes.Buffer) {
		_, shutdown, err := Setup(context.Background(), Config{Endpoint: "localhost:4317", ServiceName: "orbeat-test", SampleRatio: "not-a-number"})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		boundedShutdown(t, shutdown)
		if !strings.Contains(buf.String(), "ORBEAT_OTEL_SAMPLE_RATIO") {
			t.Errorf("expected a sample-ratio fallback warning, got: %q", buf.String())
		}
	})
}

func TestSetupDisabledIsNoop(t *testing.T) {
	ctx := context.Background()
	p, shutdown, err := Setup(ctx, Config{Endpoint: ""}) // disabled
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if p == nil {
		t.Fatal("providers nil")
	}
	_, span := p.Tracer("t").Start(ctx, "s")
	span.End()
	if span.SpanContext().IsValid() {
		t.Fatalf("disabled telemetry must produce a non-recording, invalid span context")
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown (noop): %v", err)
	}
}

func TestSetupEnabledBuildsProviders(t *testing.T) {
	ctx := context.Background()
	// bogus endpoint is fine: OTLP/gRPC connects lazily, so Setup succeeds without a live collector.
	p, shutdown, err := Setup(ctx, Config{Endpoint: "localhost:4317", ServiceName: "orbeat-test"})
	if err != nil {
		t.Fatalf("setup enabled: %v", err)
	}
	// Bound the shutdown: the collector at localhost:4317 is unreachable, so the
	// batch processor / metric reader would otherwise block on their full export
	// timeout (~20s). A bounded context is the correct production pattern too.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})
	_, span := p.Tracer("t").Start(ctx, "s")
	if !span.SpanContext().IsValid() {
		t.Fatalf("enabled telemetry must produce a valid (recording) span context")
	}
	span.End()
}
