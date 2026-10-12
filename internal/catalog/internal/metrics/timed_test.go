package metrics_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/catalog/internal/metrics"
)

type fakeInstruments struct {
	counters   map[string]int64
	histograms map[string][]float64
}

func (f *fakeInstruments) Counter(_ context.Context, name string, value int64, attrs ...attribute.KeyValue) {
	if f.counters == nil {
		f.counters = map[string]int64{}
	}
	for _, attr := range attrs {
		name += "{" + string(attr.Key) + "=" + attr.Value.AsString() + "}"
	}
	f.counters[name] += value
}

func (f *fakeInstruments) Histogram(_ context.Context, name string, value float64, _ ...attribute.KeyValue) {
	if f.histograms == nil {
		f.histograms = map[string][]float64{}
	}
	f.histograms[name] = append(f.histograms[name], value)
}

type fakeFileImporter struct {
	report core.ImportReport
	err    error
	name   string
	body   string
}

func (f *fakeFileImporter) Import(_ context.Context, r io.Reader, fileName string) (core.ImportReport, error) {
	body, _ := io.ReadAll(r)
	f.name, f.body = fileName, string(body)
	return f.report, f.err
}

func TestTimedImporterRecordsTheDurationOfEveryOutcome(t *testing.T) {
	tests := map[string]struct {
		report core.ImportReport
		err    error
	}{
		"imported":                 {core.ImportReport{Records: 97, Created: 82}, nil},
		"file rejected as a whole": {core.ImportReport{}, core.ErrInvalidHeader},
		"interrupted":              {core.ImportReport{Records: 1200, Created: 500}, context.Canceled},
		"unexpected failure":       {core.ImportReport{}, errors.New("boom")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			next := &fakeFileImporter{report: tt.report, err: tt.err}
			instruments := &fakeInstruments{}
			timed, err := metrics.NewTimedImporter(next, instruments)
			require.NoError(t, err)

			report, err := timed.Import(context.Background(), strings.NewReader("name,sku\n"), "products.csv")

			require.Equal(t, tt.report, report, "the result of the import is passed through untouched")
			require.Equal(t, tt.err, err)
			require.Equal(t, "products.csv", next.name)
			require.Equal(t, "name,sku\n", next.body)
			require.Len(t, instruments.histograms["import_duration_seconds"], 1)
			require.GreaterOrEqual(t, instruments.histograms["import_duration_seconds"][0], 0.0)
			require.Empty(t, instruments.counters, "rows are counted by the use case, not here")
		})
	}
}

func TestNewTimedImporterRequiresItsDependencies(t *testing.T) {
	_, err := metrics.NewTimedImporter(nil, &fakeInstruments{})
	require.EqualError(t, err, "catalog: timed importer requires an importer")
	_, err = metrics.NewTimedImporter(&fakeFileImporter{}, nil)
	require.EqualError(t, err, "catalog: timed importer requires instruments")
}

func TestImportRecorderCountsRowsByOutcome(t *testing.T) {
	instruments := &fakeInstruments{}
	recorder, err := metrics.NewImportRecorder(instruments)
	require.NoError(t, err)

	recorder.RecordRows(context.Background(), "created", 82)
	recorder.RecordRows(context.Background(), "created", 3)
	recorder.RecordRows(context.Background(), "rejected", 13)

	require.Equal(t, map[string]int64{
		"import_rows_total{outcome=created}":  85,
		"import_rows_total{outcome=rejected}": 13,
	}, instruments.counters)
	require.Empty(t, instruments.histograms)
}
