package observe_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/observe"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

func TestImportRecorderExposesRowsByOutcomeAndDuration(t *testing.T) {
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	provider, err := telemetry.New(t.Context(), telemetry.Config{ServiceName: "shops-test"}, log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	recorder, err := observe.NewImportRecorder(provider)
	require.NoError(t, err)

	for outcome, n := range map[string]int{"created": 82, "updated": 0, "unchanged": 0, "skipped": 2, "rejected": 13} {
		recorder.RecordRows(t.Context(), outcome, n)
	}
	recorder.RecordRows(t.Context(), "created", 3)
	instrumented, err := observe.NewInstrumentedImporter(&fakeFileImporter{}, provider, provider, rejected)
	require.NoError(t, err)
	_, err = instrumented.Import(t.Context(), strings.NewReader("name\n"), "products.csv")
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
	require.Contains(t, body, `import_duration_seconds_bucket{le="+Inf"} 1`)
	require.Contains(t, body, `import_duration_seconds_bucket{le="0.05"}`, "buckets are in seconds")
	require.NotContains(t, body, `import_duration_seconds_bucket{le="250"}`)
}

type fakeCounters struct {
	counters map[string]int64
}

func (f *fakeCounters) Counter(_ context.Context, name string, value int64, attrs ...attribute.KeyValue) {
	if f.counters == nil {
		f.counters = map[string]int64{}
	}
	for _, attr := range attrs {
		name += "{" + string(attr.Key) + "=" + attr.Value.AsString() + "}"
	}
	f.counters[name] += value
}

func TestImportRecorderCountsRowsByOutcome(t *testing.T) {
	counters := &fakeCounters{}
	recorder, err := observe.NewImportRecorder(counters)
	require.NoError(t, err)

	recorder.RecordRows(context.Background(), "created", 82)
	recorder.RecordRows(context.Background(), "created", 3)
	recorder.RecordRows(context.Background(), "rejected", 13)

	require.Equal(t, map[string]int64{
		"import_rows_total{outcome=created}":  85,
		"import_rows_total{outcome=rejected}": 13,
	}, counters.counters)
}

func TestNewImportRecorderRequiresMetrics(t *testing.T) {
	_, err := observe.NewImportRecorder(nil)
	require.EqualError(t, err, "catalog: import recorder requires counters")
}
