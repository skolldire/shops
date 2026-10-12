//go:build integration

package database_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
)

func TestQueriesAreTracedAgainstPostgres(t *testing.T) {
	_, cfg := dbtest.Start(t)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx := context.Background()
	pool, err := database.Open(ctx, cfg, database.WithTracerProvider(provider))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	tm, err := database.NewTxManager(pool)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `CREATE TABLE tracing_probe (label text PRIMARY KEY, amount integer NOT NULL CHECK (amount >= 0))`)
	require.NoError(t, err)
	already := len(recorder.Ended())

	parentCtx, parent := provider.Tracer("test").Start(ctx, "caller")
	err = tm.WithinTx(parentCtx, func(ctx context.Context) error {
		_, err := database.Q(ctx, pool).Exec(ctx, `INSERT INTO tracing_probe (label, amount) VALUES ($1, $2)`, "first", 1)
		return err
	})
	require.NoError(t, err)
	var n int
	require.NoError(t, pool.QueryRow(parentCtx, `SELECT count(*) FROM tracing_probe WHERE label = $1`, "first").Scan(&n))
	require.Equal(t, 1, n)
	_, failed := pool.Exec(parentCtx, `UPDATE tracing_probe SET amount = -1 WHERE label = 'first'`)
	require.Error(t, failed)
	parent.End()

	var names []string
	statuses := map[string]sdktrace.Status{}
	for _, span := range recorder.Ended()[already:] {
		if span.Name() == "caller" {
			continue
		}
		names = append(names, span.Name())
		statuses[span.Name()] = span.Status()
		require.Equal(t, parent.SpanContext().TraceID(), span.SpanContext().TraceID(), span.Name())
		require.Equal(t, parent.SpanContext().SpanID(), span.Parent().SpanID(), span.Name())
	}
	require.Equal(t, []string{"postgres BEGIN", "postgres INSERT", "postgres COMMIT", "postgres SELECT", "postgres UPDATE"}, names)
	require.Equal(t, codes.Unset, statuses["postgres INSERT"].Code)
	require.Equal(t, codes.Error, statuses["postgres UPDATE"].Code)
	require.Equal(t, "server rejected the request (SQLSTATE 23514)", statuses["postgres UPDATE"].Description)
}
