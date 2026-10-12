package core

import (
	"context"
	"time"
)

type ProductUpserter interface {
	Upsert(ctx context.Context, products []Product) (map[string]UpsertOutcome, error)
}

type ImportFailureReporter interface {
	BatchRetried(ctx context.Context, rows int, cause error)
	RowRejected(ctx context.Context, line int, sku string, cause error)
}

type ImportRecorder interface {
	RecordRows(ctx context.Context, outcome string, n int)
	RecordDuration(ctx context.Context, d time.Duration)
}

type Repository interface {
	Create(ctx context.Context, p Product) (Product, error)
	Get(ctx context.Context, id string) (Product, error)
	Update(ctx context.Context, id string, version int, c Changes) (Product, error)
	Delete(ctx context.Context, id string, version int) error
	Search(ctx context.Context, s Search) ([]Product, int, error)
	Categories(ctx context.Context) ([]string, error)
}
