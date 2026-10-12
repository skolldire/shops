package metrics

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/platform/telemetry"
)

const (
	importRows     = "import_rows_total"
	importDuration = "import_duration_seconds"
	outcomeKey     = "outcome"
)

type ImportRecorder struct {
	metrics telemetry.Metrics
}

func NewImportRecorder(metrics telemetry.Metrics) (*ImportRecorder, error) {
	if metrics == nil {
		return nil, errors.New("catalog: import recorder requires metrics")
	}
	return &ImportRecorder{metrics: metrics}, nil
}

func (r *ImportRecorder) RecordRows(ctx context.Context, outcome string, n int) {
	r.metrics.Counter(ctx, importRows, int64(n), attribute.String(outcomeKey, outcome))
}

func (r *ImportRecorder) RecordDuration(ctx context.Context, d time.Duration) {
	r.metrics.Histogram(ctx, importDuration, d.Seconds())
}
