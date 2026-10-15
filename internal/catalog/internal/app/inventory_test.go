package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

const (
	firstID  = "11111111-1111-4111-8111-111111111111"
	secondID = "22222222-2222-4222-8222-222222222222"
)

type fakeStock struct {
	taken, returned []domain.StockRequest
	reference       string
	paging          domain.Paging
	productID       string
}

func (f *fakeStock) TakeStock(_ context.Context, items []domain.StockRequest, reference string) ([]domain.StockLine, error) {
	f.taken, f.reference = items, reference
	return []domain.StockLine{}, nil
}

func (f *fakeStock) ReturnStock(_ context.Context, items []domain.StockRequest, reference string) error {
	f.returned, f.reference = items, reference
	return nil
}

func (f *fakeStock) Movements(_ context.Context, productID string, p domain.Paging) ([]domain.Movement, int, error) {
	f.productID, f.paging = productID, p
	return []domain.Movement{{ID: 2}, {ID: 1}}, 2, nil
}

func newInventory(t *testing.T) (*app.Inventory, *fakeStock) {
	t.Helper()
	store := &fakeStock{}
	inv, err := app.NewInventory(store)
	require.NoError(t, err)
	return inv, store
}

func TestInventorySortsAndNormalizesStockRequests(t *testing.T) {
	inv, store := newInventory(t)
	upper := "22222222-2222-4222-8222-22222222222A"
	_, err := inv.TakeStock(context.Background(), []domain.StockRequest{
		{ProductID: upper, Quantity: 2}, {ProductID: firstID, Quantity: 1},
	}, "ref-1")
	require.NoError(t, err)
	require.Equal(t, []domain.StockRequest{
		{ProductID: firstID, Quantity: 1}, {ProductID: "22222222-2222-4222-8222-22222222222a", Quantity: 2},
	}, store.taken)
	require.Equal(t, "ref-1", store.reference)

	require.NoError(t, inv.ReturnStock(context.Background(), []domain.StockRequest{
		{ProductID: secondID, Quantity: 3}, {ProductID: firstID, Quantity: 4},
	}, "ref-2"))
	require.Equal(t, []domain.StockRequest{{ProductID: firstID, Quantity: 4}, {ProductID: secondID, Quantity: 3}}, store.returned)
	require.Equal(t, "ref-2", store.reference)
}

func TestInventoryRejectsInvalidStockRequests(t *testing.T) {
	tests := map[string][]domain.StockRequest{
		"no items":          nil,
		"zero quantity":     {{ProductID: firstID, Quantity: 0}},
		"negative quantity": {{ProductID: firstID, Quantity: -1}},
		"malformed id":      {{ProductID: "not-a-uuid", Quantity: 1}},
		"repeated product":  {{ProductID: firstID, Quantity: 1}, {ProductID: secondID, Quantity: 1}, {ProductID: firstID, Quantity: 2}},
		"repeated by case":  {{ProductID: "aaaaaaaa-1111-4111-8111-111111111111", Quantity: 1}, {ProductID: "AAAAAAAA-1111-4111-8111-111111111111", Quantity: 1}},
	}
	for name, items := range tests {
		t.Run(name, func(t *testing.T) {
			inv, store := newInventory(t)
			_, err := inv.TakeStock(context.Background(), items, "ref")
			require.ErrorIs(t, err, app.ErrInvalidStockRequest)
			require.ErrorIs(t, inv.ReturnStock(context.Background(), items, "ref"), app.ErrInvalidStockRequest)
			require.Nil(t, store.taken)
			require.Nil(t, store.returned)
		})
	}
}

func TestInventoryMovements(t *testing.T) {
	inv, store := newInventory(t)
	page, err := inv.Movements(context.Background(), "11111111-1111-4111-8111-11111111111A", domain.Paging{Page: 2, PageSize: 5})
	require.NoError(t, err)
	require.Equal(t, "11111111-1111-4111-8111-11111111111a", store.productID)
	require.Equal(t, domain.Paging{Page: 2, PageSize: 5}, store.paging)
	require.Equal(t, 2, page.Total)
	require.Equal(t, 2, page.Page)
	require.Len(t, page.Items, 2)

	_, err = inv.Movements(context.Background(), "nope", domain.Paging{Page: 1, PageSize: 20})
	require.ErrorIs(t, err, app.ErrNotFound)
}

func TestNewInventoryRequiresAStore(t *testing.T) {
	_, err := app.NewInventory(nil)
	require.Error(t, err)
}

func TestImportPassesTheFileHashAsTheStockReference(t *testing.T) {
	store := &fakeStore{}
	run(t, store, csvHeader+"Alpha,A-1,,Sports,1.00,1,0.100\n")
	require.NotEmpty(t, store.references)
	for _, ref := range store.references {
		require.Equal(t, productsFile.SHA256, ref)
	}
}
