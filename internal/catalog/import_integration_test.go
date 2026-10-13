//go:build integration

package catalog_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

type sdkSpans struct {
	tracer trace.Tracer
}

func (s sdkSpans) Span(ctx context.Context, name string, fn func(ctx context.Context) error, attrs ...attribute.KeyValue) error {
	ctx, span := s.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	defer span.End()
	if err := fn(ctx); err != nil {
		span.SetStatus(codes.Error, "failed")
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func spansOf(recorder *tracetest.SpanRecorder, from int) (imports, queries []sdktrace.ReadOnlySpan) {
	for _, span := range recorder.Ended()[from:] {
		switch {
		case span.Name() == "catalog.import":
			imports = append(imports, span)
		case strings.HasPrefix(span.Name(), "postgres "):
			queries = append(queries, span)
		}
	}
	return imports, queries
}

func TestImportTraceReachesPostgres(t *testing.T) {
	_, cfg := dbtest.Start(t)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx := context.Background()
	pool, err := database.Open(ctx, cfg, database.WithTracerProvider(provider))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	module, err := catalog.NewModule(catalog.Deps{
		Pool: pool, Log: log, Metrics: telemetry.Noop(), Tracer: sdkSpans{tracer: provider.Tracer("test")},
		Import: catalog.ImportLimits{MaxBytes: 10 << 20, MaxRows: 10000},
	})
	require.NoError(t, err)
	official, err := os.ReadFile("../../testdata/products.csv")
	require.NoError(t, err)

	t.Run("the statements of an import hang from its span", func(t *testing.T) {
		from := len(recorder.Ended())

		report, err := module.Import(ctx, bytes.NewReader(official), catalog.ImportOptions{FileName: "products.csv"})

		require.NoError(t, err)
		require.Equal(t, []int{97, 2, 82, 13}, []int{report.Records, report.Skipped, report.Created, report.Rejected})
		imports, queries := spansOf(recorder, from)
		require.Len(t, imports, 1)
		require.Equal(t, codes.Ok, imports[0].Status().Code)
		require.Empty(t, imports[0].Attributes(), "no file name or content in the span")
		names := map[string]bool{}
		for _, query := range queries {
			names[query.Name()] = true
			require.Equal(t, imports[0].SpanContext().TraceID(), query.SpanContext().TraceID(), query.Name())
			require.Equal(t, imports[0].SpanContext().SpanID(), query.Parent().SpanID(), query.Name()+" is a child of catalog.import")
			require.Equal(t, codes.Unset, query.Status().Code, query.Name())
		}
		for _, want := range []string{"postgres BEGIN", "postgres SELECT", "postgres INSERT", "postgres COMMIT"} {
			require.True(t, names[want], want)
		}
	})

	t.Run("a rejected file is not a failed span and sends nothing to postgres", func(t *testing.T) {
		from := len(recorder.Ended())

		_, err := module.Import(ctx, strings.NewReader("sku,name\n"), catalog.ImportOptions{FileName: "bad.csv"})

		require.ErrorIs(t, err, catalog.ErrInvalidHeader)
		imports, queries := spansOf(recorder, from)
		require.Len(t, imports, 1)
		require.Equal(t, codes.Ok, imports[0].Status().Code, "an expected rejection does not mark the span as failed")
		require.Empty(t, queries)
	})

	t.Run("an interrupted import is a failed span", func(t *testing.T) {
		from := len(recorder.Ended())
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		body := "name,sku,description,category,price,stock,weight_kg\nKettle,KT-900,,Home,19.90,4,1.2\n"

		report, err := module.Import(cancelled, strings.NewReader(body), catalog.ImportOptions{FileName: "one.csv"})

		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, report.Records, "the report still says what was read")
		require.Zero(t, report.Created)
		imports, _ := spansOf(recorder, from)
		require.Len(t, imports, 1)
		require.Equal(t, codes.Error, imports[0].Status().Code)
	})
}
