package importlog

import (
	"context"
	"errors"
	"fmt"

	"github.com/skolldire/shops/internal/platform/logger"
)

const component = "catalog"

type Reporter struct {
	log logger.Service
}

func New(log logger.Service) (*Reporter, error) {
	if log == nil {
		return nil, errors.New("catalog: import failure reporter requires a logger")
	}
	return &Reporter{log: log}, nil
}

func (r *Reporter) BatchRetried(ctx context.Context, rows int, cause error) {
	r.log.Warn(ctx, "catalog: import batch failed, retrying row by row", map[string]any{
		"component": component, "rows": rows, "cause": fmt.Sprint(cause),
	})
}

func (r *Reporter) RowRejected(ctx context.Context, line int, sku string, cause error) {
	r.log.Error(ctx, fmt.Errorf("catalog: import row rejected by the database: %w", cause), map[string]any{
		"component": component, "line": line, "sku": sku,
	})
}
