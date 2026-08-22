// Package telemetry wires OpenTelemetry traces + metrics for orbeat. It exports
// via OTLP/gRPC to a collector when ORBEAT_OTEL_ENDPOINT is set; otherwise it
// installs no-op providers (zero overhead) — the default. Everything else in
// orbeat reads the global tracer/meter/propagator this package installs.
package telemetry

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// Must track the semconv version `resource.Default()` is built with, which
	// the SDK bumps on its own schedule (1.41.0 -> 1.43.0 in otel 1.45.0).
	// `resource.Merge` REFUSES to merge resources with conflicting schema URLs,
	// so a stale import here is not a cosmetic mismatch — Setup() returns
	// "conflicting Schema URL" and telemetry fails to initialise entirely.
	// Pinned by internal/telemetry's own tests, which is how the otel 1.45 bump
	// was caught. When bumping otel, check this line.
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/credentials"
)

// Config controls whether and how telemetry is exported. An empty Endpoint
// disables telemetry: Setup installs no-op providers with zero overhead.
type Config struct {
	Endpoint       string
	ServiceName    string
	ServiceVersion string
	SampleRatio    string // parsed via ParseSampleRatio; invalid/out-of-range → 1.0
	// Insecure controls whether the OTLP/gRPC exporters skip TLS. Empty
	// defaults to true (matches the historical hard-coded WithInsecure()
	// behavior, so unset deployments are unaffected); parsed via ParseInsecure.
	Insecure string
}

// ParseSampleRatio parses cfg.SampleRatio as the trace sampling ratio. It
// falls back to 1.0 (sample everything) on any malformed or out-of-range
// ([0,1]) value, so a bad config can never silently produce a nonsensical
// ratio. ok is false when the fallback was used (the caller should warn) —
// mirrors config.Config.MarketplaceGitTimeoutDuration's (value, ok) pattern.
func (c Config) ParseSampleRatio() (ratio float64, ok bool) {
	r, err := strconv.ParseFloat(c.SampleRatio, 64)
	if err != nil || r < 0 || r > 1 {
		return 1.0, false
	}
	return r, true
}

// ParseInsecure parses cfg.Insecure as a bool controlling whether the OTLP/
// gRPC exporters skip TLS. An empty value defaults to true (back-compat: the
// exporters were unconditionally insecure before this field existed) and is
// reported as ok — that is the intended default, not a malformed value. Any
// other unparseable value falls back to true (never silently upgrades to TLS
// on a typo, which would just as silently break export against a TLS-only
// collector) and is reported as NOT ok, so the caller can warn.
func (c Config) ParseInsecure() (insecure bool, ok bool) {
	if c.Insecure == "" {
		return true, true
	}
	b, err := strconv.ParseBool(c.Insecure)
	if err != nil {
		return true, false
	}
	return b, true
}

// Providers exposes the installed (or no-op) global tracer/meter, plus a pgx
// query tracer for database instrumentation.
type Providers struct{ enabled bool }

func (Providers) Tracer(name string) trace.Tracer { return otel.Tracer(name) }
func (Providers) Meter(name string) metric.Meter  { return otel.Meter(name) }

// QueryTracer returns the pgx query tracer (no-op when disabled, since it reads
// the global tracer which is then a no-op). See pgxtrace.go for the queryTracer
// implementation.
func (Providers) QueryTracer() pgx.QueryTracer { return &queryTracer{tr: otel.Tracer("orbeat/db")} }

// Setup installs the global OTel tracer/meter/propagator. When cfg.Endpoint is
// empty, telemetry is disabled: the returned Providers reads the (untouched)
// global no-op providers, and shutdown is a no-op. Otherwise it builds an
// OTLP/gRPC exporting pipeline and installs it globally.
func Setup(ctx context.Context, cfg Config) (*Providers, func(context.Context) error, error) {
	if cfg.Endpoint == "" {
		return &Providers{enabled: false}, func(context.Context) error { return nil }, nil
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	))
	if err != nil {
		return nil, nil, fmt.Errorf("otel resource: %w", err)
	}

	insecure, insecureOK := cfg.ParseInsecure()
	if !insecureOK {
		slog.Warn("ORBEAT_OTEL_INSECURE is not a valid bool; falling back to insecure=true",
			"raw", cfg.Insecure)
	}
	if insecure {
		slog.Warn("OTel OTLP/gRPC exporter is running WITHOUT TLS; set ORBEAT_OTEL_INSECURE=false for a production collector",
			"endpoint", cfg.Endpoint)
	}
	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	if insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	} else {
		// System root CA pool: passing an empty tls.Config lets crypto/tls fill
		// RootCAs from the host's trust store, and grpc derives ServerName from
		// the dial target — the standard "verify against public/enterprise CA"
		// posture, no bespoke cert plumbing required.
		creds := credentials.NewTLS(&tls.Config{})
		traceOpts = append(traceOpts, otlptracegrpc.WithTLSCredentials(creds))
		metricOpts = append(metricOpts, otlpmetricgrpc.WithTLSCredentials(creds))
	}

	traceExp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	ratio, ratioOK := cfg.ParseSampleRatio()
	if !ratioOK {
		slog.Warn("ORBEAT_OTEL_SAMPLE_RATIO is not a valid ratio in [0,1]; falling back to 1.0 (sample everything)",
			"raw", cfg.SampleRatio)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	metricExp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		otel.Handle(err)
	}
	shutdown := func(ctx context.Context) error {
		e1 := tp.Shutdown(ctx)
		e2 := mp.Shutdown(ctx)
		if e1 != nil {
			return e1
		}
		return e2
	}
	return &Providers{enabled: true}, shutdown, nil
}
