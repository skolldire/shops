package observe_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/observe"
)

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

func TestRowImporterWrapsTheUseCaseInASpan(t *testing.T) {
	boom := errors.New("boom")
	file := app.ImportFile{Name: "products.csv", Bytes: 8930, SHA256: "6fc46b84"}
	rows := []app.ImportRow{{Line: 2}, {Line: 3}, {Line: 4}}
	for name, wantErr := range map[string]error{"applied": nil, "interrupted": context.Canceled, "failed": boom} {
		t.Run(name, func(t *testing.T) {
			want := app.ImportReport{File: file, Records: 3, Created: 2}
			next := &fakeRows{report: want, err: wantErr}
			spans := &fakeSpans{}
			traced, err := observe.NewRowImporter(next, spans)
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

func TestNewRowImporterRequiresItsDependencies(t *testing.T) {
	_, err := observe.NewRowImporter(nil, &fakeSpans{})
	require.EqualError(t, err, "catalog: traced row importer requires an importer")
	_, err = observe.NewRowImporter(&fakeRows{}, nil)
	require.EqualError(t, err, "catalog: traced row importer requires spans")
}
