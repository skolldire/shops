package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestHTTPMiddleware(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	p, _ := newTestProvider(t, exporter)
	var fields map[string]any
	h := p.HTTPMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields = TraceFields(r.Context())
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.NoError(t, p.tracerProvider.ForceFlush(t.Context()))

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "GET", spans[0].Name)
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", spans[0].SpanContext.TraceID().String())
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", fields["trace_id"])
	require.Equal(t, spans[0].SpanContext.SpanID().String(), fields["span_id"])

	body := scrape(t, p)
	require.Contains(t, body, "http_server_request_duration_seconds_count")
	require.Contains(t, body, `http_response_status_code="418"`)
}

func TestTraceFieldsWithoutSpan(t *testing.T) {
	require.Empty(t, TraceFields(context.Background()))
}

func TestHTTPMetricsIgnoreClientHost(t *testing.T) {
	p, _ := newTestProvider(t, nil)
	h := p.HTTPMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, host := range []string{"a.example", "b.example:8443", "c.example:65000"} {
		req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
		req.Host = host
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	for _, method := range []string{"BREW", "PROPFIND-X"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/health/live", nil))
	}

	body := scrape(t, p)
	require.Contains(t, body, `http_request_method="_OTHER"`)
	for _, leak := range []string{"a.example", "b.example", "c.example", "8443", "65000", "server_address", "server_port", "BREW", "PROPFIND"} {
		require.NotContains(t, body, leak)
	}
	require.Contains(t, body, `http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="204",network_protocol_name="http",network_protocol_version="1.1",url_scheme="http"} 3`)
}
