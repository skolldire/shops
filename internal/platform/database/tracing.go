package database

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	tracerName       = "github.com/skolldire/shops/internal/platform/database"
	unknownOperation = "QUERY"
)

var operations = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "WITH": true, "TRUNCATE": true,
	"BEGIN": true, "COMMIT": true, "ROLLBACK": true, "SAVEPOINT": true, "RELEASE": true,
}

type Option func(*pgxpool.Config)

func WithTracerProvider(provider trace.TracerProvider) Option {
	return func(pc *pgxpool.Config) {
		if provider != nil {
			pc.ConnConfig.Tracer = newQueryTracer(provider)
		}
	}
}

type queryTracer struct {
	tracer trace.Tracer
}

type querySpanKey struct{}

func newQueryTracer(provider trace.TracerProvider) *queryTracer {
	return &queryTracer{tracer: provider.Tracer(tracerName)}
}

func (q *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	operation := operationOf(data.SQL)
	ctx, span := q.tracer.Start(ctx, "postgres "+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(semconv.DBSystemNamePostgreSQL, semconv.DBOperationName(operation)),
	)
	return context.WithValue(ctx, querySpanKey{}, span)
}

func (q *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(querySpanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows) {
		span.SetStatus(codes.Error, describe(data.Err))
	}
	span.End()
}

func operationOf(sql string) string {
	fields := strings.Fields(sql)
	if len(fields) == 0 {
		return unknownOperation
	}
	if word := strings.ToUpper(fields[0]); operations[word] {
		return word
	}
	return unknownOperation
}
