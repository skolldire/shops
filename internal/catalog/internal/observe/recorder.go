package observe

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/app"
)

const (
	importRows = "import_rows_total"
	outcomeKey = "outcome"
)

type Counters interface {
	Counter(ctx context.Context, name string, value int64, attrs ...attribute.KeyValue)
}

type ImportRecorder struct {
	counters Counters
}

func NewImportRecorder(counters Counters) (*ImportRecorder, error) {
	if counters == nil {
		return nil, errors.New("catalog: import recorder requires counters")
	}
	for _, outcome := range app.ImportRowOutcomes() {
		counters.Counter(context.Background(), importRows, 0, attribute.String(outcomeKey, outcome))
	}
	return &ImportRecorder{counters: counters}, nil
}

func (r *ImportRecorder) RecordRows(ctx context.Context, outcome string, n int) {
	r.counters.Counter(ctx, importRows, int64(n), attribute.String(outcomeKey, outcome))
}
