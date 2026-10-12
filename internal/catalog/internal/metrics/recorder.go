package metrics

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
)

const (
	importRows     = "import_rows_total"
	importDuration = "import_duration_seconds"
	outcomeKey     = "outcome"
)

type Instruments interface {
	Counter(ctx context.Context, name string, value int64, attrs ...attribute.KeyValue)
	Histogram(ctx context.Context, name string, value float64, attrs ...attribute.KeyValue)
}

type ImportRecorder struct {
	instruments Instruments
}

func NewImportRecorder(instruments Instruments) (*ImportRecorder, error) {
	if instruments == nil {
		return nil, errors.New("catalog: import recorder requires metrics")
	}
	return &ImportRecorder{instruments: instruments}, nil
}

func (r *ImportRecorder) RecordRows(ctx context.Context, outcome string, n int) {
	r.instruments.Counter(ctx, importRows, int64(n), attribute.String(outcomeKey, outcome))
}
