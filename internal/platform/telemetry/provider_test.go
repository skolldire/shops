package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/skolldire/shops/internal/platform/logger"
)

func newTestProvider(t *testing.T, exporter sdktrace.SpanExporter) (*Provider, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "debug"}, &logs)
	require.NoError(t, err)
	p, err := newProvider(Config{ServiceName: "shops-test", ServiceVersion: "0.0.1"}, log, exporter)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p, &logs
}

func scrape(t *testing.T, p *Provider) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(body)
}

func TestMetricsAreExposedInPrometheusFormat(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	attr := attribute.String("outcome", "ok")

	p.Counter(t.Context(), "orders_total", 2, attr)
	p.Counter(t.Context(), "orders_total", 1, attr)
	p.Gauge(t.Context(), "stock_level", 7)
	p.Histogram(t.Context(), "checkout_duration_seconds", 0.25)

	body := scrape(t, p)
	require.Contains(t, body, `orders_total{outcome="ok"} 3`)
	require.Contains(t, body, `stock_level 7`)
	require.Contains(t, body, `checkout_duration_seconds_count 1`)
	require.Contains(t, body, `target_info{service_name="shops-test",service_version="0.0.1"} 1`)
	require.Contains(t, body, "go_goroutines")
}

func TestInvalidInstrumentIsLoggedNotPanicking(t *testing.T) {
	p, logs := newTestProvider(t, nil)

	p.Counter(t.Context(), "", 1)

	require.Contains(t, logs.String(), "telemetry: instrument unavailable")
}

func TestSpans(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	p, _ := newTestProvider(t, exporter)
	boom := errors.New("declined")

	require.NoError(t, p.Span(t.Context(), "ok-span", func(context.Context) error { return nil }, attribute.String("k", "v")))
	require.ErrorIs(t, p.Span(t.Context(), "failed-span", func(context.Context) error { return boom }), boom)
	require.NoError(t, p.tracerProvider.ForceFlush(t.Context()))

	spans := exporter.GetSpans()
	require.Len(t, spans, 2)
	require.Equal(t, "ok-span", spans[0].Name)
	require.Equal(t, codes.Ok, spans[0].Status.Code)
	require.Contains(t, spans[0].Attributes, attribute.String("k", "v"))
	require.Equal(t, codes.Error, spans[1].Status.Code)
}

func TestTracingDisabledStillRunsSpans(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	called := false

	err := p.Span(t.Context(), "noop", func(context.Context) error { called = true; return nil })

	require.NoError(t, err)
	require.True(t, called)
}

func TestNewValidates(t *testing.T) {
	log, err := logger.New(logger.Config{Level: "info"}, io.Discard)
	require.NoError(t, err)

	_, err = New(t.Context(), Config{}, log)
	require.EqualError(t, err, "telemetry: service_name is required")
	_, err = New(t.Context(), Config{ServiceName: "x"}, nil)
	require.EqualError(t, err, "telemetry: logger is required")
}

var _ Telemetry = (*Provider)(nil)
