//go:build integration

package ordering_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
)

func TestAFailedReservationLeavesNoPurchaseStockChangeOrMovement(t *testing.T) {
	f := setup(t, newProvider(approve))
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `
		CREATE FUNCTION fail_boom_items() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.sku = 'BOOM-1' THEN
				RAISE EXCEPTION 'synthetic failure while saving the purchase items';
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER purchase_items_boom BEFORE INSERT ON purchase_items
		FOR EACH ROW EXECUTE FUNCTION fail_boom_items();`)
	require.NoError(t, err)

	cases := []struct {
		name    string
		prepare func(t *testing.T) []catalog.Product
		want    error
		cause   string
	}{
		{"the price changed", func(t *testing.T) []catalog.Product {
			a, b := f.product(t, "PRICE-1", "29.99", 5), f.product(t, "PRICE-2", "1.00", 5)
			f.fillCart(t, "alice", map[string]int{a.ID: 1, b.ID: 1})
			newPrice := decimal.RequireFromString("39.99")
			_, err := f.catalog.Update(ctx, a.ID, catalog.UpdateInput{Version: a.Version, Price: &newPrice})
			require.NoError(t, err)
			return []catalog.Product{a, b}
		}, ordering.ErrPriceChanged, ""},
		{"the stock is short", func(t *testing.T) []catalog.Product {
			a, b := f.product(t, "SHORT-1", "2.00", 5), f.product(t, "SHORT-2", "3.00", 1)
			f.fillCart(t, "alice", map[string]int{a.ID: 2, b.ID: 1})
			require.NoError(t, f.orders.SetQuantity(ctx, "alice", b.ID, 2))
			return []catalog.Product{a, b}
		}, ordering.ErrInsufficientStock, ""},
		{"a product was deleted", func(t *testing.T) []catalog.Product {
			a, b := f.product(t, "GONE-1", "2.00", 5), f.product(t, "GONE-2", "3.00", 5)
			f.fillCart(t, "alice", map[string]int{a.ID: 1, b.ID: 1})
			require.NoError(t, f.catalog.Delete(ctx, b.ID, b.Version))
			return []catalog.Product{a, b}
		}, ordering.ErrProductUnavailable, ""},
		{"the database fails after the stock was taken", func(t *testing.T) []catalog.Product {
			a, b := f.product(t, "AAA-1", "2.00", 5), f.product(t, "BOOM-1", "3.00", 5)
			f.fillCart(t, "alice", map[string]int{a.ID: 2, b.ID: 1})
			return []catalog.Product{a, b}
		}, nil, "synthetic failure while saving the purchase items"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			products := c.prepare(t)
			before := map[string]int{}
			for _, p := range products {
				before[p.ID] = f.stock(t, p.ID)
			}
			movements := f.count(t, `SELECT count(*) FROM inventory_movements`)

			_, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
			require.Error(t, err)
			if c.want != nil {
				require.ErrorIs(t, err, c.want)
			}
			require.ErrorContains(t, err, c.cause)

			for _, p := range products {
				require.Equal(t, before[p.ID], f.stock(t, p.ID), p.SKU)
				require.NoError(t, f.orders.RemoveFromCart(ctx, "alice", p.ID))
			}
			require.Equal(t, movements, f.count(t, `SELECT count(*) FROM inventory_movements`))
			require.Zero(t, f.count(t, `SELECT count(*) FROM purchases`))
			require.Zero(t, f.count(t, `SELECT count(*) FROM purchase_items`))
			require.Empty(t, f.pay.attempts, "nothing is charged")
		})
	}
}
