package observe

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/app"
)

const (
	importApplySpan = "catalog.import.apply"
	importRowsKey   = "catalog.import.rows"
)

type RowImporter interface {
	Import(ctx context.Context, file app.ImportFile, rows []app.ImportRow) (app.ImportReport, error)
}

type TracedRowImporter struct {
	next  RowImporter
	spans Spans
}

func NewRowImporter(next RowImporter, spans Spans) (*TracedRowImporter, error) {
	switch {
	case next == nil:
		return nil, errors.New("catalog: traced row importer requires an importer")
	case spans == nil:
		return nil, errors.New("catalog: traced row importer requires spans")
	}
	return &TracedRowImporter{next: next, spans: spans}, nil
}

func (t *TracedRowImporter) Import(ctx context.Context, file app.ImportFile, rows []app.ImportRow) (app.ImportReport, error) {
	var (
		report    app.ImportReport
		importErr error
	)
	_ = t.spans.Span(ctx, importApplySpan, func(ctx context.Context) error {
		report, importErr = t.next.Import(ctx, file, rows)
		return importErr
	}, attribute.Int(importRowsKey, len(rows)))
	return report, importErr
}
