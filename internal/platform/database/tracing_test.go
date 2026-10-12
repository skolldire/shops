package database

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func recordedTracer(t *testing.T) (*queryTracer, *tracetest.SpanRecorder, trace.Tracer) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return newQueryTracer(provider), recorder, provider.Tracer("test")
}

func attributes(span sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, kv := range span.Attributes() {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func TestQueryTracerNamesSpansAfterTheOperation(t *testing.T) {
	tests := map[string]string{
		"SELECT sku FROM products WHERE sku = $1": "SELECT",
		"  select 1": "SELECT",
		"\nINSERT INTO products (sku) VALUES ($1)": "INSERT",
		"UPDATE products SET stock = $1":           "UPDATE",
		"delete from products":                     "DELETE",
		"begin":                                    "BEGIN",
		"commit":                                   "COMMIT",
		"rollback":                                 "ROLLBACK",
		"WITH t AS (SELECT 1) SELECT * FROM t":     "WITH",
		"TRUNCATE products":                        "TRUNCATE",
		"":                                         "QUERY",
		"   ":                                      "QUERY",
		"/* hello */ SELECT 1":                     "QUERY",
		"select-something-odd":                     "QUERY",
		"VACUUM products; DROP TABLE products":     "QUERY",
		"SELECT * FROM users WHERE email = 'someone@example.com'": "SELECT",
	}
	for sql, operation := range tests {
		t.Run(operation+" "+sql, func(t *testing.T) {
			tracer, recorder, _ := recordedTracer(t)

			ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: sql, Args: []any{"secret-value"}})
			tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})

			spans := recorder.Ended()
			require.Len(t, spans, 1)
			require.Equal(t, "postgres "+operation, spans[0].Name())
			require.Equal(t, trace.SpanKindClient, spans[0].SpanKind())
			require.Equal(t, map[string]string{"db.system.name": "postgresql", "db.operation.name": operation}, attributes(spans[0]))
			require.Equal(t, codes.Unset, spans[0].Status().Code)
		})
	}
}

func TestQueryTracerKeepsStatementsAndArgumentsOutOfTheSpan(t *testing.T) {
	tracer, recorder, _ := recordedTracer(t)

	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{
		SQL: "SELECT * FROM users WHERE email = 'someone@example.com'", Args: []any{"4111111111111111"},
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})

	span := recorder.Ended()[0]
	for _, kv := range span.Attributes() {
		require.NotContains(t, kv.Value.String(), "example.com")
		require.NotContains(t, kv.Value.String(), "4111")
	}
	require.NotContains(t, span.Name(), "users")
	require.Empty(t, span.Events())
}

func TestQueryTracerMarksFailuresWithoutLeakingDetails(t *testing.T) {
	tests := map[string]struct {
		err    error
		status string
	}{
		"constraint violation": {&pgconn.PgError{Code: "23514", Message: "new row violates check constraint \"products_price_check\"", Detail: "Failing row contains (secret)."}, "server rejected the request (SQLSTATE 23514)"},
		"cancelled":            {context.Canceled, "canceled"},
		"deadline":             {context.DeadlineExceeded, "timed out"},
		"anything else":        {errors.New("dial tcp 10.0.0.5:5432: connection refused"), "connection failed"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tracer, recorder, _ := recordedTracer(t)

			ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "INSERT INTO products VALUES ($1)"})
			tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: tt.err})

			span := recorder.Ended()[0]
			require.Equal(t, codes.Error, span.Status().Code)
			require.Equal(t, tt.status, span.Status().Description)
			require.NotContains(t, span.Status().Description, "10.0.0.5")
			require.NotContains(t, span.Status().Description, "secret")
			require.Empty(t, span.Events())
		})
	}
}

func TestQueryTracerDoesNotTreatNoRowsAsAFailure(t *testing.T) {
	tracer, recorder, _ := recordedTracer(t)

	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: pgx.ErrNoRows})

	require.Equal(t, codes.Unset, recorder.Ended()[0].Status().Code)
}

func TestQueryTracerNestsUnderTheCallerSpan(t *testing.T) {
	tracer, recorder, caller := recordedTracer(t)
	parentCtx, parent := caller.Start(context.Background(), "catalog.import")

	first := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tracer.TraceQueryEnd(first, nil, pgx.TraceQueryEndData{})
	second := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "INSERT INTO t VALUES (1)"})
	tracer.TraceQueryEnd(second, nil, pgx.TraceQueryEndData{})
	parent.End()

	spans := recorder.Ended()
	require.Len(t, spans, 3)
	for _, span := range spans[:2] {
		require.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID(), span.Name())
		require.Equal(t, parent.SpanContext().TraceID(), span.SpanContext().TraceID())
	}
	require.Equal(t, "catalog.import", spans[2].Name())
}

func TestQueryTracerEndWithoutStartDoesNothing(t *testing.T) {
	tracer, recorder, _ := recordedTracer(t)

	require.NotPanics(t, func() {
		tracer.TraceQueryEnd(context.Background(), nil, pgx.TraceQueryEndData{Err: errors.New("boom")})
	})
	require.Empty(t, recorder.Ended())
}

func TestWithTracerProviderInstallsTheTracerOnThePool(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	base := Config{Host: "db", Port: 5432, Name: "shop", User: "shop", SSLMode: "disable", MaxConns: 2, ConnectTimeout: 1}

	plain, err := poolConfig(base)
	require.NoError(t, err)
	require.Nil(t, plain.ConnConfig.Tracer)

	traced, err := poolConfig(base, WithTracerProvider(provider))
	require.NoError(t, err)
	require.IsType(t, &queryTracer{}, traced.ConnConfig.Tracer)

	ignored, err := poolConfig(base, WithTracerProvider(nil))
	require.NoError(t, err)
	require.Nil(t, ignored.ConnConfig.Tracer)
}
