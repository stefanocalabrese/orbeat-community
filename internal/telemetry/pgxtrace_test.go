package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestQueryTracerEmitsSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	qt := &queryTracer{tr: tp.Tracer("orbeat/db")}
	ctx := qt.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	qt.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("boom")})

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	s := spans[0]
	if s.Name != "db.query" {
		t.Fatalf("span name = %q", s.Name)
	}
	var sawStmt bool
	for _, a := range s.Attributes {
		if a.Key == "db.statement" && a.Value.AsString() == "SELECT 1" {
			sawStmt = true
		}
	}
	if !sawStmt {
		t.Fatalf("db.statement attr missing: %+v", s.Attributes)
	}
	if s.Status.Code.String() != "Error" {
		t.Fatalf("errored query should set span status Error, got %v", s.Status.Code)
	}
}
