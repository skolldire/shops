package metrics_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/metrics"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

func TestImportRecorderExposesRowsByOutcomeAndDuration(t *testing.T) {
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	provider, err := telemetry.New(t.Context(), telemetry.Config{ServiceName: "shops-test"}, log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	recorder, err := metrics.NewImportRecorder(provider)
	require.NoError(t, err)

	for outcome, n := range map[string]int{"created": 82, "updated": 0, "unchanged": 0, "skipped": 2, "rejected": 13} {
		recorder.RecordRows(t.Context(), outcome, n)
	}
	recorder.RecordRows(t.Context(), "created", 3)
	timed, err := metrics.NewTimedImporter(&fakeFileImporter{}, provider)
	require.NoError(t, err)
	_, err = timed.Import(t.Context(), strings.NewReader("name\n"), "products.csv")
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	provider.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	require.Contains(t, body, `import_rows_total{outcome="created"} 85`)
	require.Contains(t, body, `import_rows_total{outcome="updated"} 0`)
	require.Contains(t, body, `import_rows_total{outcome="unchanged"} 0`)
	require.Contains(t, body, `import_rows_total{outcome="skipped"} 2`)
	require.Contains(t, body, `import_rows_total{outcome="rejected"} 13`)
	require.Contains(t, body, `import_duration_seconds_count 1`)
	require.Contains(t, body, `import_duration_seconds_bucket{le="0.05"} 1`)
	require.Contains(t, body, `import_duration_seconds_bucket{le="10"} 1`)
	require.NotContains(t, body, `import_duration_seconds_bucket{le="250"}`)
}

func TestNewImportRecorderRequiresMetrics(t *testing.T) {
	_, err := metrics.NewImportRecorder(nil)
	require.EqualError(t, err, "catalog: import recorder requires metrics")
}
