package app

import (
	"context"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

type Tx interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

type ProductUpserter interface {
	Upsert(ctx context.Context, mode domain.ImportMode, products []domain.Product, reference string) (UpsertResult, error)
	LockExisting(ctx context.Context, skus []string) error
}

type ImportFailureReporter interface {
	BatchRetried(ctx context.Context, rows int, cause error)
	RowRejected(ctx context.Context, line int, sku string, cause error)
}

type ImportRecorder interface {
	RecordRows(ctx context.Context, outcome string, n int)
}

type Repository interface {
	Create(ctx context.Context, p domain.Product) (domain.Product, error)
	Get(ctx context.Context, id string) (domain.Product, error)
	Update(ctx context.Context, id string, version int, c domain.Changes) (domain.Product, error)
	Delete(ctx context.Context, id string, version int) error
	Search(ctx context.Context, s domain.Search) ([]domain.Product, int, error)
	Categories(ctx context.Context) ([]string, error)
}

type StockStore interface {
	ReserveStock(ctx context.Context, items []domain.StockRequest, reference string) ([]domain.StockLine, error)
	ReleaseStock(ctx context.Context, items []domain.StockRequest, reference string) error
	CommitStock(ctx context.Context, items []domain.StockRequest, reference string) error
	Movements(ctx context.Context, productID string, p domain.Paging) ([]domain.Movement, int, error)
}
