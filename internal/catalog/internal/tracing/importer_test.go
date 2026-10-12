package tracing_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/tracing"
)

type spanKey struct{}

type recordedSpan struct {
	name  string
	attrs map[string]string
	err   error
}

type fakeSpans struct {
	spans []recordedSpan
}

func (f *fakeSpans) Span(ctx context.Context, name string, fn func(ctx context.Context) error, attrs ...attribute.KeyValue) error {
	recorded := recordedSpan{name: name, attrs: map[string]string{}}
	for _, kv := range attrs {
		recorded.attrs[string(kv.Key)] = kv.Value.String()
	}
	recorded.err = fn(context.WithValue(ctx, spanKey{}, name))
	f.spans = append(f.spans, recorded)
	return recorded.err
}

type fakeFiles struct {
	report app.ImportReport
	err    error
	inSpan string
	name   string
	body   string
}

func (f *fakeFiles) Import(ctx context.Context, r io.Reader, fileName string) (app.ImportReport, error) {
	body, _ := io.ReadAll(r)
	f.inSpan, _ = ctx.Value(spanKey{}).(string)
	f.name, f.body = fileName, string(body)
	return f.report, f.err
}

type fakeRows struct {
	report app.ImportReport
	err    error
	inSpan string
	file   app.ImportFile
	rows   []app.ImportRow
}

func (f *fakeRows) Import(ctx context.Context, file app.ImportFile, rows []app.ImportRow) (app.ImportReport, error) {
	f.inSpan, _ = ctx.Value(spanKey{}).(string)
	f.file, f.rows = file, rows
	return f.report, f.err
}

var errRejected = errors.New("the file was rejected")

func rejected(err error) bool { return errors.Is(err, errRejected) }

func TestFileImporterWrapsTheImportInASpan(t *testing.T) {
	boom := errors.New("boom")
	tests := map[string]struct {
		report    app.ImportReport
		err       error
		spanError error
	}{
		"imported":           {app.ImportReport{Records: 97, Created: 82}, nil, nil},
		"file rejected":      {app.ImportReport{}, errRejected, nil},
		"wrapped rejection":  {app.ImportReport{}, errors.Join(errors.New("context"), errRejected), nil},
		"interrupted":        {app.ImportReport{Records: 1200, Created: 500}, context.Canceled, context.Canceled},
		"unexpected failure": {app.ImportReport{}, boom, boom},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			next := &fakeFiles{report: tt.report, err: tt.err}
			spans := &fakeSpans{}
			traced, err := tracing.NewFileImporter(next, spans, rejected)
			require.NoError(t, err)

			report, err := traced.Import(context.Background(), strings.NewReader("name,sku\n"), "products.csv")

			require.Equal(t, tt.report, report, "the result of the import is passed through untouched")
			require.Equal(t, tt.err, err, "the caller still gets the error, also when the span is not marked as failed")
			require.Equal(t, "products.csv", next.name)
			require.Equal(t, "name,sku\n", next.body)
			require.Equal(t, "catalog.import", next.inSpan, "the import runs inside the span")
			require.Len(t, spans.spans, 1)
			require.Equal(t, "catalog.import", spans.spans[0].name)
			require.Empty(t, spans.spans[0].attrs, "no file name or content in the span")
			require.Equal(t, tt.spanError, spans.spans[0].err, "a rejected file is an expected answer, not a failed span")
		})
	}
}

func TestRowImporterWrapsTheUseCaseInASpan(t *testing.T) {
	boom := errors.New("boom")
	file := app.ImportFile{Name: "products.csv", Bytes: 8930, SHA256: "6fc46b84"}
	rows := []app.ImportRow{{Line: 2}, {Line: 3}, {Line: 4}}
	for name, wantErr := range map[string]error{"applied": nil, "interrupted": context.Canceled, "failed": boom} {
		t.Run(name, func(t *testing.T) {
			want := app.ImportReport{File: file, Records: 3, Created: 2}
			next := &fakeRows{report: want, err: wantErr}
			spans := &fakeSpans{}
			traced, err := tracing.NewRowImporter(next, spans)
			require.NoError(t, err)

			report, err := traced.Import(context.Background(), file, rows)

			require.Equal(t, want, report)
			require.Equal(t, wantErr, err)
			require.Equal(t, file, next.file)
			require.Equal(t, rows, next.rows)
			require.Equal(t, "catalog.import.apply", next.inSpan)
			require.Len(t, spans.spans, 1)
			require.Equal(t, "catalog.import.apply", spans.spans[0].name)
			require.Equal(t, map[string]string{"catalog.import.rows": "3"}, spans.spans[0].attrs, "only a bounded count, never names or SKUs")
			require.Equal(t, wantErr, spans.spans[0].err)
		})
	}
}

func TestConstructorsRequireTheirDependencies(t *testing.T) {
	_, err := tracing.NewFileImporter(nil, &fakeSpans{}, rejected)
	require.EqualError(t, err, "catalog: traced file importer requires an importer")
	_, err = tracing.NewFileImporter(&fakeFiles{}, nil, rejected)
	require.EqualError(t, err, "catalog: traced file importer requires spans")
	_, err = tracing.NewFileImporter(&fakeFiles{}, &fakeSpans{}, nil)
	require.EqualError(t, err, "catalog: traced file importer requires a rejection test")

	_, err = tracing.NewRowImporter(nil, &fakeSpans{})
	require.EqualError(t, err, "catalog: traced row importer requires an importer")
	_, err = tracing.NewRowImporter(&fakeRows{}, nil)
	require.EqualError(t, err, "catalog: traced row importer requires spans")
}
