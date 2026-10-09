package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

func testApp(t *testing.T) *app {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "host=127.0.0.1 port=1 user=x dbname=x sslmode=disable connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	cfg := shippedConfig(t)
	tel, err := telemetry.New(t.Context(), cfg.Telemetry, log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	a, err := buildApp(cfg, log, pool, tel)
	require.NoError(t, err)
	return a
}

func TestBuildAppRoutes(t *testing.T) {
	a := testApp(t)
	tests := []struct {
		method, path, contentType string
		status                    int
	}{
		{http.MethodGet, "/health/live", "application/json", 200},
		{http.MethodGet, "/health/ready", "application/json", 503},
		{http.MethodDelete, "/health/live", "application/problem+json", 405},
		{http.MethodGet, "/health/nothing", "application/problem+json", 404},
		{http.MethodGet, "/api/v1/nothing", "application/problem+json", 404},
		{http.MethodPut, "/api/v1/categories", "application/problem+json", 405},
		{http.MethodGet, "/no-existe", "text/html; charset=utf-8", 404},
		{http.MethodGet, "/metrics", "text/html; charset=utf-8", 404},
		{http.MethodGet, "/static/app.css", "text/css; charset=utf-8", 200},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			a.api.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			require.Equal(t, tt.status, rec.Code)
			require.Equal(t, tt.contentType, rec.Header().Get("Content-Type"))
			require.NotEmpty(t, rec.Header().Get("X-Request-ID"))
			if strings.HasPrefix(tt.contentType, "application/") {
				require.True(t, json.Valid(rec.Body.Bytes()), rec.Body.String())
			}
		})
	}
	rec := httptest.NewRecorder()
	a.api.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/health/live", nil))
	require.Equal(t, "GET", rec.Header().Get("Allow"))

	rec = httptest.NewRecorder()
	a.api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/no-existe", nil))
	require.NotEmpty(t, rec.Header().Get("Content-Security-Policy"))
}

func TestMetricsServedSeparately(t *testing.T) {
	a := testApp(t)
	a.api.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health/live", nil))
	rec := httptest.NewRecorder()

	a.metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "http_server_request_duration_seconds")
	require.Contains(t, rec.Body.String(), `target_info{service_name="shops"`)
}

func TestContextFieldsIncludeRequestID(t *testing.T) {
	var fields map[string]any
	h := httpx.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		fields = contextFields(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req1")

	h.ServeHTTP(httptest.NewRecorder(), req)

	require.Equal(t, map[string]any{"request_id": "req1"}, fields)
	require.Empty(t, contextFields(context.Background()))
}

func TestResolveConfigPath(t *testing.T) {
	require.Equal(t, "/flag.yaml", resolveConfigPath("/flag.yaml", "/env.yaml"))
	require.Equal(t, "/env.yaml", resolveConfigPath("", "/env.yaml"))
	require.Equal(t, defaultConfigPath, resolveConfigPath("", ""))
}

func TestProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ok.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()

	require.NoError(t, probe(t.Context(), ok.URL, time.Second))
	require.ErrorContains(t, probe(t.Context(), down.URL, time.Second), "unexpected status 503")
	require.ErrorIs(t, probe(t.Context(), slow.URL, 50*time.Millisecond), context.DeadlineExceeded)
}
