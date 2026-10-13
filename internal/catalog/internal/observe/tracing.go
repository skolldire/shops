package observe

import (
	"context"
	"errors"
	"io"

	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/app"
)

const (
	importSpan      = "catalog.import"
	importApplySpan = "catalog.import.apply"
	importRowsKey   = "catalog.import.rows"
)

type Spans interface {
	Span(ctx context.Context, name string, fn func(ctx context.Context) error, attrs ...attribute.KeyValue) error
}

type FileImporter interface {
	Import(ctx context.Context, r io.Reader, fileName string) (app.ImportReport, error)
}

type RowImporter interface {
	Import(ctx context.Context, file app.ImportFile, rows []app.ImportRow) (app.ImportReport, error)
}

type TracedFileImporter struct {
	next     FileImporter
	spans    Spans
	rejected func(error) bool
}

func NewFileImporter(next FileImporter, spans Spans, rejected func(error) bool) (*TracedFileImporter, error) {
	switch {
	case next == nil:
		return nil, errors.New("catalog: traced file importer requires an importer")
	case spans == nil:
		return nil, errors.New("catalog: traced file importer requires spans")
	case rejected == nil:
		return nil, errors.New("catalog: traced file importer requires a rejection test")
	}
	return &TracedFileImporter{next: next, spans: spans, rejected: rejected}, nil
}

func (t *TracedFileImporter) Import(ctx context.Context, r io.Reader, fileName string) (app.ImportReport, error) {
	var (
		report    app.ImportReport
		importErr error
	)
	_ = t.spans.Span(ctx, importSpan, func(ctx context.Context) error {
		report, importErr = t.next.Import(ctx, r, fileName)
		if t.rejected(importErr) {
			return nil
		}
		return importErr
	})
	return report, importErr
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
