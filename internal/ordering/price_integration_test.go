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

func TestARecoveredPurchaseChargesTheStoredAmountAfterAPriceChange(t *testing.T) {
	f := setup(t, newProvider(unknownAfterCharge))
	ctx := context.Background()
	a, b := f.product(t, "A-1", "29.99", 5), f.product(t, "B-1", "5.00", 5)
	f.fillCart(t, "alice", map[string]int{a.ID: 2, b.ID: 1})
	res, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.NoError(t, err)
	require.Equal(t, ordering.StatusPending, res.Purchase.Status)
	require.Equal(t, "64.98", res.Purchase.Total.StringFixed(2))

	report, err := f.catalog.Import(ctx, strings.NewReader(
		"name,sku,description,category,price,stock,weight_kg\nProduct A-1,A-1,,Tests,39.99,3,1\n"), catalog.ImportOptions{FileName: "prices.csv"})
	require.NoError(t, err)
	require.Equal(t, 1, report.Updated)
	current, err := f.catalog.Get(ctx, b.ID)
	require.NoError(t, err)
	price := decimal.RequireFromString("7.50")
	_, err = f.catalog.Update(ctx, b.ID, catalog.UpdateInput{Version: current.Version, Price: &price})
	require.NoError(t, err)

	f.pay.set(approve)
	recovered, err := f.orders.RecoverPending(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, recovered.Confirmed)

	attempts := f.pay.attemptsFor(res.Purchase.ID)
	require.Len(t, attempts, 2)
	for _, at := range attempts {
		require.Equal(t, "64.98", at.Amount.StringFixed(2), "the stored amount, not the new prices")
	}
	p, err := f.orders.Purchase(ctx, "alice", res.Purchase.ID)
	require.NoError(t, err)
	require.Equal(t, ordering.StatusConfirmed, p.Status)
	require.Equal(t, "64.98", p.Total.StringFixed(2))
	for _, it := range p.Items {
		want := map[string]string{"A-1": "29.99", "B-1": "5.00"}[it.SKU]
		require.Equal(t, want, it.UnitPrice.StringFixed(2), it.SKU)
	}
	f.requireAttemptsMatchThePurchases(t)
	f.requireMoneyInvariants(t)
}
