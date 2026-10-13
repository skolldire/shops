package observe

import (
	"context"
	"errors"
	"fmt"

	"github.com/skolldire/shops/internal/platform/logger"
)

const component = "catalog"

type FailureReporter struct {
	log logger.Service
}

func NewFailureReporter(log logger.Service) (*FailureReporter, error) {
	if log == nil {
		return nil, errors.New("catalog: import failure reporter requires a logger")
	}
	return &FailureReporter{log: log}, nil
}

func (r *FailureReporter) BatchRetried(ctx context.Context, rows int, cause error) {
	r.log.Warn(ctx, "catalog: import batch failed, retrying row by row", map[string]any{
		"component": component, "rows": rows, "cause": fmt.Sprint(cause),
	})
}

func (r *FailureReporter) RowRejected(ctx context.Context, line int, sku string, cause error) {
	r.log.Error(ctx, fmt.Errorf("catalog: import row rejected by the database: %w", cause), map[string]any{
		"component": component, "line": line, "sku": sku,
	})
}
