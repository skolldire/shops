package health_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/health"
)

func TestLiveIgnoresCheckers(t *testing.T) {
	svc, _ := newService(t, time.Second)
	svc.Register("postgres", failing("down"))
	rec := httptest.NewRecorder()

	svc.Live(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{"status":"up"}`, rec.Body.String())
}

func TestReady(t *testing.T) {
	tests := map[string]struct {
		checker health.Checker
		code    int
		body    string
	}{
		"up":   {ok(), http.StatusOK, `{"status":"up","checks":{"postgres":"up"}}`},
		"down": {failing("dial tcp 10.0.0.5:5432: connection refused"), http.StatusServiceUnavailable, `{"status":"down","checks":{"postgres":"down"}}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			svc, _ := newService(t, time.Second)
			svc.Register("postgres", tt.checker)
			rec := httptest.NewRecorder()

			svc.Ready(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

			require.Equal(t, tt.code, rec.Code)
			require.JSONEq(t, tt.body, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "10.0.0.5")
		})
	}
}
