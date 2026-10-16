//go:build integration

package ordering_test

import (
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
)

func (f fixture) importCSV(t *testing.T, rows string) catalog.ImportReport {
	t.Helper()
	report, err := f.catalog.Import(context.Background(),
		strings.NewReader("name,sku,description,category,price,stock,weight_kg\n"+rows), catalog.ImportOptions{FileName: "stock.csv"})
	require.NoError(t, err)
	return report
}

func (f fixture) requireMovementsRebuildTheStock(t *testing.T) {
	t.Helper()
	require.Zero(t, f.count(t, `
		SELECT count(*) FROM products p
		WHERE p.stock <> (SELECT COALESCE(sum(m.delta), 0) FROM inventory_movements m WHERE m.product_id = p.id)
		   OR p.reserved <> (SELECT COALESCE(sum(m.reserved_delta), 0) FROM inventory_movements m WHERE m.product_id = p.id)`))
}

func TestAReplaceImportDuringAReservationKeepsTheFileStock(t *testing.T) {
	for name, tc := range map[string]struct {
		token          string
		recovered      ordering.RecoveryReport
		stock          int
		sold, released int
	}{
		"then approved": {"pm_approved", ordering.RecoveryReport{Confirmed: 1}, 9, 1, 0},
		"then declined": {"pm_declined", ordering.RecoveryReport{Declined: 1}, 10, 0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, newProvider(unknownAfterCharge))
			ctx := context.Background()
			a := f.product(t, "A-1", "10.00", 5)
			require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 1))
			res, err := f.orders.CheckoutCart(ctx, "alice", key, tc.token)
			require.NoError(t, err)
			require.Equal(t, ordering.StatusPending, res.Purchase.Status)
			require.Equal(t, []int{5, 1}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)})

			report := f.importCSV(t, "Product A-1,A-1,,Tests,10.00,10,1\n")
			require.Equal(t, 1, report.Updated)
			require.Equal(t, []int{10, 1}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)}, "replace sets the physical units and keeps the reservation")

			f.pay.set(approve)
			recovered, err := f.recoverPending(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.recovered, recovered)
			require.Equal(t, []int{tc.stock, 0}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)},
				"the file said 10 units: a sale leaves 9 and a decline 10, never 11")
			require.Equal(t, tc.sold, f.movements(t, "sale"))
			require.Equal(t, tc.released, f.movements(t, "reservation_release"))
			f.requireMovementsRebuildTheStock(t)
		})
	}
}

func TestAnAddImportDuringAReservationAddsToThePhysicalStock(t *testing.T) {
	for name, tc := range map[string]struct {
		token          string
		recovered      ordering.RecoveryReport
		stock          int
		sold, released int
	}{
		"then approved": {"pm_approved", ordering.RecoveryReport{Confirmed: 1}, 14, 1, 0},
		"then declined": {"pm_declined", ordering.RecoveryReport{Declined: 1}, 15, 0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, newProvider(unknownAfterCharge))
			ctx := context.Background()
			a := f.product(t, "A-1", "10.00", 5)
			require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 1))
			res, err := f.orders.CheckoutCart(ctx, "alice", key, tc.token)
			require.NoError(t, err)
			require.Equal(t, ordering.StatusPending, res.Purchase.Status)

			report, err := f.catalog.Import(ctx, strings.NewReader("name,sku,description,category,price,stock,weight_kg\nProduct A-1,A-1,,Tests,10.00,10,1\n"),
				catalog.ImportOptions{FileName: "delivery.csv", Mode: catalog.ImportAdd, IdempotencyKey: otherKey})
			require.NoError(t, err)
			require.Equal(t, 1, report.Updated)
			require.Equal(t, []int{15, 1}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)}, "add sums the delivery to the physical units and keeps the reservation")

			f.pay.set(approve)
			recovered, err := f.recoverPending(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.recovered, recovered)
			require.Equal(t, []int{tc.stock, 0}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)},
				"5 units plus a delivery of 10: a sale leaves 14 and a decline 15")
			require.Equal(t, tc.sold, f.movements(t, "sale"))
			require.Equal(t, tc.released, f.movements(t, "reservation_release"))
			require.Equal(t, 1, f.movements(t, "import_add"))
			f.requireMovementsRebuildTheStock(t)
		})
	}
}

func TestAReservationDoesNotInvalidateAnOpenEdit(t *testing.T) {
	f := setup(t, newProvider(unknownAfterCharge))
	ctx := context.Background()
	a := f.product(t, "A-1", "10.00", 5)
	opened, err := f.catalog.Get(ctx, a.ID)
	require.NoError(t, err)

	require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 2))
	res, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.NoError(t, err)
	require.Equal(t, ordering.StatusPending, res.Purchase.Status)

	name, price := "Renamed", decimal.RequireFromString("12.00")
	updated, err := f.catalog.Update(ctx, a.ID, catalog.UpdateInput{Version: opened.Version, Name: &name, Price: &price})
	require.NoError(t, err, "the reservation did not change the version the form was opened with")
	require.Equal(t, []int{5, 2, 3}, []int{updated.Stock, updated.Reserved, updated.Available})

	f.pay.set(approve)
	_, err = f.recoverPending(ctx)
	require.NoError(t, err)
	_, err = f.catalog.Update(ctx, a.ID, catalog.UpdateInput{Version: updated.Version, Name: &name})
	require.ErrorIs(t, err, catalog.ErrVersionConflict, "a sale changes the physical stock, so it does change the version")
}

func TestLoweringTheStockBelowTheReservedUnitsChangesNothing(t *testing.T) {
	f := setup(t, newProvider(unknownAfterCharge))
	ctx := context.Background()
	a := f.product(t, "A-1", "10.00", 5)
	require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 3))
	_, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.NoError(t, err)
	movements := f.count(t, `SELECT count(*) FROM inventory_movements`)

	name, price, stock := "Renamed", decimal.RequireFromString("12.00"), int64(2)
	_, err = f.catalog.Update(ctx, a.ID, catalog.UpdateInput{Version: a.Version, Name: &name, Price: &price, Stock: &stock})
	var below *catalog.BelowReservedError
	require.ErrorAs(t, err, &below)
	require.Equal(t, 3, below.Reserved)
	require.Equal(t, "must be at least 3, the units reserved by pending purchases", below.Message)

	current, err := f.catalog.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "Product A-1", current.Name)
	require.Equal(t, "10.00", current.Price.StringFixed(2))
	require.Equal(t, []int{5, 3, a.Version}, []int{current.Stock, current.Reserved, current.Version})
	require.Equal(t, movements, f.count(t, `SELECT count(*) FROM inventory_movements`))

	report := f.importCSV(t, "Product A-1,A-1,,Tests,10.00,2,1\n")
	require.Equal(t, 1, report.Rejected)
	require.Equal(t, "below_reserved", report.Errors[0].Code)
	require.Equal(t, []int{5, 3}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)})
}

func TestRetriesDoNotRepeatReleasesSalesOrMovements(t *testing.T) {
	for name, token := range map[string]string{"approved": "pm_approved", "declined": "pm_declined"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, newProvider(unknownAfterCharge))
			ctx := context.Background()
			a := f.product(t, "A-1", "10.00", 5)
			require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 2))
			_, err := f.orders.CheckoutCart(ctx, "alice", key, token)
			require.NoError(t, err)
			f.pay.set(approve)
			for range 3 {
				_, err := f.recoverPending(ctx)
				require.NoError(t, err)
			}
			require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 2))
			if token == "pm_declined" {
				require.NoError(t, f.orders.SetQuantity(ctx, "alice", a.ID, 2))
			}
			replayed, err := f.orders.CheckoutCart(ctx, "alice", key, token)
			require.NoError(t, err)
			require.Equal(t, ordering.Replayed, replayed.Kind)

			require.Equal(t, 1, f.movements(t, "reservation"))
			require.Equal(t, f.movements(t, "sale")+f.movements(t, "reservation_release"), 1)
			require.Zero(t, f.reserved(t, a.ID))
			f.requireMovementsRebuildTheStock(t)
		})
	}
}
