package observe_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/observe"
)

type spanKey struct{}

type requestKey struct{}

type recordedSpan struct {
	name  string
	attrs map[string]string
	err   error
}

type fakeSpans struct {
	spans  []recordedSpan
	events *[]string
}

func (f *fakeSpans) Span(ctx context.Context, name string, fn func(ctx context.Context) error, attrs ...attribute.KeyValue) error {
	recorded := recordedSpan{name: name, attrs: map[string]string{}}
	for _, kv := range attrs {
		recorded.attrs[string(kv.Key)] = kv.Value.String()
	}
	f.note("span opened")
	recorded.err = fn(context.WithValue(ctx, spanKey{}, name))
	f.note("span closed")
	f.spans = append(f.spans, recorded)
	return recorded.err
}

func (f *fakeSpans) note(event string) {
	if f.events != nil {
		*f.events = append(*f.events, event)
	}
}

type fakeHistograms struct {
	histograms map[string][]float64
	contexts   []context.Context
	events     *[]string
}

func (f *fakeHistograms) Histogram(ctx context.Context, name string, value float64, _ ...attribute.KeyValue) {
	if f.histograms == nil {
		f.histograms = map[string][]float64{}
	}
	f.histograms[name] = append(f.histograms[name], value)
	f.contexts = append(f.contexts, ctx)
	if f.events != nil {
		*f.events = append(*f.events, "duration recorded")
	}
}

type fakeFileImporter struct {
	report    app.ImportReport
	err       error
	name      string
	body      string
	inSpan    string
	cancelled bool
	events    *[]string
}

func (f *fakeFileImporter) Import(ctx context.Context, r io.Reader, fileName string) (app.ImportReport, error) {
	body, _ := io.ReadAll(r)
	f.name, f.body = fileName, string(body)
	f.inSpan, _ = ctx.Value(spanKey{}).(string)
	f.cancelled = ctx.Err() != nil
	if f.events != nil {
		*f.events = append(*f.events, "import ran")
	}
	return f.report, f.err
}

var errRejected = errors.New("the file was rejected")

func rejected(err error) bool { return errors.Is(err, errRejected) }

func TestInstrumentedImporterObservesEveryOutcomeWithoutChangingIt(t *testing.T) {
	boom := errors.New("boom")
	tests := map[string]struct {
		report    app.ImportReport
		err       error
		cancelled bool
		spanError error
	}{
		"imported":               {app.ImportReport{Records: 97, Created: 82}, nil, false, nil},
		"early rejection":        {app.ImportReport{}, errRejected, false, nil},
		"wrapped rejection":      {app.ImportReport{}, errors.Join(errors.New("reading"), errRejected), false, nil},
		"cancelled with partial": {app.ImportReport{Records: 1200, Created: 500}, context.Canceled, true, context.Canceled},
		"technical failure":      {app.ImportReport{}, boom, false, boom},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			next := &fakeFileImporter{report: tt.report, err: tt.err}
			spans := &fakeSpans{}
			histograms := &fakeHistograms{}
			instrumented, err := observe.NewInstrumentedImporter(next, spans, histograms, rejected)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), requestKey{}, "req-1"))
			defer cancel()
			if tt.cancelled {
				cancel()
			}

			report, err := instrumented.Import(ctx, strings.NewReader("name,sku\n"), "products.csv")

			require.Equal(t, tt.report, report, "the report is passed through untouched")
			require.Equal(t, tt.err, err, "the caller gets the error also when the span is not marked as failed")
			require.Equal(t, "products.csv", next.name)
			require.Equal(t, "name,sku\n", next.body)
			require.Equal(t, tt.cancelled, next.cancelled, "the import itself still sees the cancellation")
			require.Equal(t, "catalog.import", next.inSpan, "the import runs inside the span")

			require.Len(t, spans.spans, 1)
			require.Equal(t, "catalog.import", spans.spans[0].name)
			require.Empty(t, spans.spans[0].attrs, "no file name or content in the span")
			require.Equal(t, tt.spanError, spans.spans[0].err, "an expected rejection does not fail the span, a technical failure does")

			require.Len(t, histograms.histograms, 1, "nothing but the duration is recorded")
			require.Len(t, histograms.histograms["import_duration_seconds"], 1, "the duration is recorded once for every outcome")
			require.GreaterOrEqual(t, histograms.histograms["import_duration_seconds"][0], 0.0)
			require.NoError(t, histograms.contexts[0].Err(), "the duration is recorded even when the request was cancelled")
			require.Equal(t, "req-1", histograms.contexts[0].Value(requestKey{}), "the values of the request context are kept")
		})
	}
}

func TestInstrumentedImporterRecordsTheDurationAfterTheSpanCloses(t *testing.T) {
	var events []string
	next := &fakeFileImporter{events: &events}
	instrumented, err := observe.NewInstrumentedImporter(next, &fakeSpans{events: &events}, &fakeHistograms{events: &events}, rejected)
	require.NoError(t, err)

	_, err = instrumented.Import(context.Background(), strings.NewReader("name\n"), "products.csv")

	require.NoError(t, err)
	require.Equal(t, []string{"span opened", "import ran", "span closed", "duration recorded"}, events)
}

func TestNewInstrumentedImporterRequiresItsDependencies(t *testing.T) {
	_, err := observe.NewInstrumentedImporter(nil, &fakeSpans{}, &fakeHistograms{}, rejected)
	require.EqualError(t, err, "catalog: instrumented importer requires an importer")
	_, err = observe.NewInstrumentedImporter(&fakeFileImporter{}, nil, &fakeHistograms{}, rejected)
	require.EqualError(t, err, "catalog: instrumented importer requires spans")
	_, err = observe.NewInstrumentedImporter(&fakeFileImporter{}, &fakeSpans{}, nil, rejected)
	require.EqualError(t, err, "catalog: instrumented importer requires histograms")
	_, err = observe.NewInstrumentedImporter(&fakeFileImporter{}, &fakeSpans{}, &fakeHistograms{}, nil)
	require.EqualError(t, err, "catalog: instrumented importer requires a rejection test")
}
