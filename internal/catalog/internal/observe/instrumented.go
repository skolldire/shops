package observe

import (
	"context"
	"errors"
	"io"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/app"
)

const (
	importSpan     = "catalog.import"
	importDuration = "import_duration_seconds"
)

type FileImporter interface {
	Import(ctx context.Context, r io.Reader, opts app.ImportOptions) (app.ImportReport, error)
}

type Spans interface {
	Span(ctx context.Context, name string, fn func(ctx context.Context) error, attrs ...attribute.KeyValue) error
}

type Histograms interface {
	Histogram(ctx context.Context, name string, value float64, attrs ...attribute.KeyValue)
}

type InstrumentedImporter struct {
	next       FileImporter
	spans      Spans
	histograms Histograms
	rejected   func(error) bool
}

func NewInstrumentedImporter(next FileImporter, spans Spans, histograms Histograms, rejected func(error) bool) (*InstrumentedImporter, error) {
	switch {
	case next == nil:
		return nil, errors.New("catalog: instrumented importer requires an importer")
	case spans == nil:
		return nil, errors.New("catalog: instrumented importer requires spans")
	case histograms == nil:
		return nil, errors.New("catalog: instrumented importer requires histograms")
	case rejected == nil:
		return nil, errors.New("catalog: instrumented importer requires a rejection test")
	}
	return &InstrumentedImporter{next: next, spans: spans, histograms: histograms, rejected: rejected}, nil
}

func (i *InstrumentedImporter) Import(ctx context.Context, r io.Reader, opts app.ImportOptions) (app.ImportReport, error) {
	start := time.Now()
	defer func() {
		i.histograms.Histogram(context.WithoutCancel(ctx), importDuration, time.Since(start).Seconds())
	}()

	var (
		report    app.ImportReport
		importErr error
	)
	_ = i.spans.Span(ctx, importSpan, func(ctx context.Context) error {
		report, importErr = i.next.Import(ctx, r, opts)
		if i.rejected(importErr) {
			return nil
		}
		return importErr
	})
	return report, importErr
}
