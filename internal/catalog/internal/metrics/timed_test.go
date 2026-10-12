package metrics_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/metrics"
)

type fakeCounters struct {
	counters map[string]int64
}

type fakeHistograms struct {
	histograms map[string][]float64
	contexts   []context.Context
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

func (f *fakeHistograms) Histogram(ctx context.Context, name string, value float64, _ ...attribute.KeyValue) {
	if f.histograms == nil {
		f.histograms = map[string][]float64{}
	}
	f.histograms[name] = append(f.histograms[name], value)
	f.contexts = append(f.contexts, ctx)
}

type requestKey struct{}

type fakeFileImporter struct {
	report    app.ImportReport
	err       error
	name      string
	body      string
	cancelled bool
}

func (f *fakeFileImporter) Import(ctx context.Context, r io.Reader, fileName string) (app.ImportReport, error) {
	body, _ := io.ReadAll(r)
	f.name, f.body = fileName, string(body)
	f.cancelled = ctx.Err() != nil
	return f.report, f.err
}

func TestTimedImporterRecordsTheDurationOfEveryOutcome(t *testing.T) {
	tests := map[string]struct {
		report    app.ImportReport
		err       error
		cancelled bool
	}{
		"imported":                 {app.ImportReport{Records: 97, Created: 82}, nil, false},
		"file rejected as a whole": {app.ImportReport{}, errors.New("the file was rejected"), false},
		"interrupted":              {app.ImportReport{Records: 1200, Created: 500}, context.Canceled, true},
		"unexpected failure":       {app.ImportReport{}, errors.New("boom"), false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			next := &fakeFileImporter{report: tt.report, err: tt.err}
			histograms := &fakeHistograms{}
			timed, err := metrics.NewTimedImporter(next, histograms)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), requestKey{}, "req-1"))
			defer cancel()
			if tt.cancelled {
				cancel()
			}

			report, err := timed.Import(ctx, strings.NewReader("name,sku\n"), "products.csv")

			require.Equal(t, tt.report, report, "the result of the import is passed through untouched")
			require.Equal(t, tt.err, err)
			require.Equal(t, "products.csv", next.name)
			require.Equal(t, "name,sku\n", next.body)
			require.Len(t, histograms.histograms["import_duration_seconds"], 1)
			require.GreaterOrEqual(t, histograms.histograms["import_duration_seconds"][0], 0.0)
			require.Len(t, histograms.histograms, 1, "the decorator records nothing else")
			require.Len(t, histograms.contexts, 1)
			require.NoError(t, histograms.contexts[0].Err(), "the duration is recorded even when the request was cancelled")
			require.Equal(t, "req-1", histograms.contexts[0].Value(requestKey{}), "the values of the request context are kept")
			require.Equal(t, tt.cancelled, next.cancelled, "the import itself still sees the cancellation")
		})
	}
}

func TestNewTimedImporterRequiresItsDependencies(t *testing.T) {
	_, err := metrics.NewTimedImporter(nil, &fakeHistograms{})
	require.EqualError(t, err, "catalog: timed importer requires an importer")
	_, err = metrics.NewTimedImporter(&fakeFileImporter{}, nil)
	require.EqualError(t, err, "catalog: timed importer requires histograms")
}

func TestImportRecorderCountsRowsByOutcome(t *testing.T) {
	counters := &fakeCounters{}
	recorder, err := metrics.NewImportRecorder(counters)
	require.NoError(t, err)

	recorder.RecordRows(context.Background(), "created", 82)
	recorder.RecordRows(context.Background(), "created", 3)
	recorder.RecordRows(context.Background(), "rejected", 13)

	require.Equal(t, map[string]int64{
		"import_rows_total{outcome=created}":  85,
		"import_rows_total{outcome=rejected}": 13,
	}, counters.counters)
}
