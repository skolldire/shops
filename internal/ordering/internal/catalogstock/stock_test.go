package catalogstock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/catalogstock"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

const id = "11111111-1111-4111-8111-111111111111"

type fakeCatalog struct {
	err       error
	requests  []catalog.StockRequest
	reference string
	product   catalog.Product
}

func (f *fakeCatalog) TakeStock(_ context.Context, items []catalog.StockRequest, reference string) ([]catalog.StockLine, error) {
	f.requests, f.reference = items, reference
	if f.err != nil {
		return nil, f.err
	}
	return []catalog.StockLine{{ProductID: id, SKU: "A-1", Name: "Alpha", Price: decimal.RequireFromString("2.50"), Quantity: 2}}, nil
}

func (f *fakeCatalog) ReturnStock(_ context.Context, items []catalog.StockRequest, reference string) error {
	f.requests, f.reference = items, reference
	return f.err
}

func (f *fakeCatalog) Get(context.Context, string) (catalog.Product, error) {
	return f.product, f.err
}

func TestStockTranslatesTheCatalog(t *testing.T) {
	c := &fakeCatalog{}
	s, err := catalogstock.New(c)
	require.NoError(t, err)
	ctx := context.Background()

	lines, err := s.Take(ctx, []domain.Quantity{{ProductID: id, Quantity: 2}}, "purchase-1")
	require.NoError(t, err)
	require.Equal(t, []domain.ReservedLine{{ProductID: id, SKU: "A-1", Name: "Alpha", Price: decimal.RequireFromString("2.50"), Quantity: 2}}, lines)
	require.Equal(t, []catalog.StockRequest{{ProductID: id, Quantity: 2}}, c.requests)
	require.Equal(t, "purchase-1", c.reference)

	require.NoError(t, s.Return(ctx, []domain.Quantity{{ProductID: id, Quantity: 2}}, "purchase-1"))

	c.err = &catalog.ProductUnavailableError{ProductID: id}
	_, err = s.Take(ctx, []domain.Quantity{{ProductID: id, Quantity: 2}}, "p")
	var unavailable *app.UnavailableProductError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, domain.ID(id), unavailable.ProductID)

	c.err = &catalog.InsufficientStockError{ProductID: id, Available: 1}
	_, err = s.Take(ctx, []domain.Quantity{{ProductID: id, Quantity: 2}}, "p")
	var short *app.InsufficientStockError
	require.ErrorAs(t, err, &short)
	require.Equal(t, 1, short.Available)

	boom := errors.New("deadlock")
	c.err = boom
	_, err = s.Take(ctx, []domain.Quantity{{ProductID: id, Quantity: 2}}, "p")
	require.ErrorIs(t, err, boom)
	require.ErrorIs(t, s.Return(ctx, nil, "p"), boom)

	c.err = catalog.ErrNotFound
	_, err = s.Product(ctx, id)
	require.ErrorIs(t, err, app.ErrProductUnavailable)

	c.err = nil
	c.product = catalog.Product{ID: id, SKU: "A-1", Name: "Alpha", Price: decimal.RequireFromString("3"), Stock: 4}
	p, err := s.Product(ctx, id)
	require.NoError(t, err)
	require.Equal(t, app.Product{ID: id, SKU: "A-1", Name: "Alpha", Price: decimal.RequireFromString("3"), Stock: 4}, p)

	_, err = catalogstock.New(nil)
	require.Error(t, err)
}
