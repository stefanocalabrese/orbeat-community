package telemetry

import (
	"context"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// queryTracer implements pgx.QueryTracer, emitting one span per query. It reads
// the global tracer, so it's a no-op when telemetry is disabled. orbeat's SQL is
// static and arg-free ($1 placeholders), so recording the SQL text leaks no
// argument values; args are never recorded.
type queryTracer struct{ tr trace.Tracer }

type pgxSpanKey struct{}

func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := data.SQL
	if len(sql) > 512 {
		sql = sql[:512]
	}
	ctx, span := t.tr.Start(ctx, "db.query",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "postgresql"),
			attribute.String("db.statement", sql),
		),
	)
	return context.WithValue(ctx, pgxSpanKey{}, span)
}

func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(pgxSpanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, data.Err.Error())
	}
	span.End()
}
