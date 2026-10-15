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
	reserved, released, committed []domain.StockRequest
	reference                     string
	paging                        domain.Paging
	productID                     string
}

func (f *fakeStock) ReserveStock(_ context.Context, items []domain.StockRequest, reference string) ([]domain.StockLine, error) {
	f.reserved, f.reference = items, reference
	return []domain.StockLine{}, nil
}

func (f *fakeStock) ReleaseStock(_ context.Context, items []domain.StockRequest, reference string) error {
	f.released, f.reference = items, reference
	return nil
}

func (f *fakeStock) CommitStock(_ context.Context, items []domain.StockRequest, reference string) error {
	f.committed, f.reference = items, reference
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

func TestInventorySortsReservationsReleasesAndCommits(t *testing.T) {
	inv, store := newInventory(t)
	ctx := context.Background()
	unsorted := []domain.StockRequest{{ProductID: "22222222-2222-4222-8222-22222222222A", Quantity: 2}, {ProductID: firstID, Quantity: 1}}
	sorted := []domain.StockRequest{{ProductID: firstID, Quantity: 1}, {ProductID: "22222222-2222-4222-8222-22222222222a", Quantity: 2}}

	_, err := inv.ReserveStock(ctx, unsorted, "purchase-1")
	require.NoError(t, err)
	require.Equal(t, sorted, store.reserved)
	require.NoError(t, inv.ReleaseStock(ctx, unsorted, "purchase-2"))
	require.Equal(t, sorted, store.released)
	require.NoError(t, inv.CommitStock(ctx, unsorted, "purchase-3"))
	require.Equal(t, sorted, store.committed)
	require.Equal(t, "purchase-3", store.reference)
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
			_, err := inv.ReserveStock(context.Background(), items, "ref")
			require.ErrorIs(t, err, app.ErrInvalidStockRequest)
			require.ErrorIs(t, inv.ReleaseStock(context.Background(), items, "ref"), app.ErrInvalidStockRequest)
			require.ErrorIs(t, inv.CommitStock(context.Background(), items, "ref"), app.ErrInvalidStockRequest)
			require.Nil(t, store.reserved)
			require.Nil(t, store.released)
			require.Nil(t, store.committed)
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
