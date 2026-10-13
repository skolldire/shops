package observe

import (
	"context"
	"errors"
	"io"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/app"
)

type Histograms interface {
	Histogram(ctx context.Context, name string, value float64, attrs ...attribute.KeyValue)
}

type TimedImporter struct {
	next       FileImporter
	histograms Histograms
}

func NewTimedImporter(next FileImporter, histograms Histograms) (*TimedImporter, error) {
	switch {
	case next == nil:
		return nil, errors.New("catalog: timed importer requires an importer")
	case histograms == nil:
		return nil, errors.New("catalog: timed importer requires histograms")
	}
	return &TimedImporter{next: next, histograms: histograms}, nil
}

func (t *TimedImporter) Import(ctx context.Context, r io.Reader, fileName string) (app.ImportReport, error) {
	start := time.Now()
	defer func() {
		t.histograms.Histogram(context.WithoutCancel(ctx), importDuration, time.Since(start).Seconds())
	}()
	return t.next.Import(ctx, r, fileName)
}
