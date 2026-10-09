package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
)

func newTestRouter(t *testing.T) (*chi.Mux, *bytes.Buffer) {
	t.Helper()
	log, logs := newLogger(t)
	r := httpx.NewRouter(log)
	r.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "up"})
	})
	api := chi.NewRouter()
	api.Get("/v1/products/{id}", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": chi.URLParam(r, "id")})
	})
	api.Delete("/v1/products/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Mount("/api", api)
	return r, logs
}

func serve(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestRouterMatchesRoutes(t *testing.T) {
	r, logs := newTestRouter(t)

	rec := serve(r, http.MethodGet, "/api/v1/products/42")

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"id":"42"}`, rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("X-Request-ID"))
	require.Contains(t, logs.String(), `"route":"/api/v1/products/{id}"`)
}

func TestRouterNotFound(t *testing.T) {
	r, _ := newTestRouter(t)

	for _, path := range []string{"/no-existe", "/api/v1/nothing"} {
		rec := serve(r, http.MethodGet, path)

		require.Equal(t, http.StatusNotFound, rec.Code, path)
		p := decodeProblem(t, rec)
		require.Equal(t, "not_found", p.Code)
		require.Equal(t, path, p.Instance)
		require.NotEmpty(t, p.RequestID)
	}
}

func TestRouterMethodNotAllowed(t *testing.T) {
	r, _ := newTestRouter(t)
	tests := map[string]string{
		"/health/live":        "GET",
		"/api/v1/products/42": "GET, DELETE",
	}
	for path, allow := range tests {
		rec := serve(r, http.MethodPost, path)

		require.Equal(t, http.StatusMethodNotAllowed, rec.Code, path)
		require.Equal(t, allow, rec.Header().Get("Allow"), path)
		require.Equal(t, "method_not_allowed", decodeProblem(t, rec).Code)
	}
}

func TestRouterNamesSpanAfterRoute(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	r, _ := newTestRouter(t)
	h := otelhttp.NewHandler(r, "http.server", otelhttp.WithTracerProvider(tp))

	serve(h, http.MethodGet, "/api/v1/products/42")

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, "GET /api/v1/products/{id}", spans[0].Name)
	require.Contains(t, spans[0].Attributes, attribute.String("http.route", "/api/v1/products/{id}"))
}

func TestRouterLabelsMetricsWithRoute(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	r, _ := newTestRouter(t)
	h := otelhttp.NewHandler(r, "http.server", otelhttp.WithMeterProvider(mp))

	serve(h, http.MethodGet, "/api/v1/products/42")
	serve(h, http.MethodGet, "/api/v1/products/43")
	serve(h, http.MethodGet, "/no-existe")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	routes := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				route, _ := dp.Attributes.Value("http.route")
				routes[route.AsString()] += dp.Count
			}
		}
	}
	require.Equal(t, map[string]uint64{"/api/v1/products/{id}": 2, "": 1}, routes)
}

func TestRouterLogsPanickingRequests(t *testing.T) {
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs, logger.WithContextExtractor(func(ctx context.Context) map[string]any {
		return map[string]any{"request_id": httpx.RequestIDFrom(ctx)}
	}))
	require.NoError(t, err)
	r := httpx.NewRouter(log)
	r.Get("/api/v1/explode/{id}", func(http.ResponseWriter, *http.Request) { panic("boom") })

	rec := serve(r, http.MethodGet, "/api/v1/explode/7")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var access map[string]any
	panicked := false
	for dec := json.NewDecoder(&logs); dec.More(); {
		var line map[string]any
		require.NoError(t, dec.Decode(&line))
		switch line["message"] {
		case "http request":
			access = line
		case "panic recovered: boom":
			panicked = true
		}
	}
	require.True(t, panicked, "missing panic log line")
	require.NotNil(t, access, "missing access log line")
	require.EqualValues(t, 500, access["status"])
	require.Equal(t, "/api/v1/explode/{id}", access["route"])
	require.NotEmpty(t, rec.Header().Get("X-Request-ID"))
	require.Equal(t, rec.Header().Get("X-Request-ID"), access["request_id"])
}
