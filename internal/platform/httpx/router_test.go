package httpx_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/skolldire/shops/internal/platform/httpx"
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
