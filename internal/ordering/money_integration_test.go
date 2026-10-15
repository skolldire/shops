//go:build integration

package ordering_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering"
	"github.com/skolldire/shops/internal/payment"
)

const thirdKey = "8f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"

func (p *provider) attemptsFor(reference string) []payment.ChargeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []payment.ChargeRequest
	for _, a := range p.attempts {
		if a.Reference == reference {
			out = append(out, a)
		}
	}
	return out
}

func (f fixture) requireMoneyInvariants(t *testing.T) {
	t.Helper()
	require.Zero(t, f.count(t, `
		SELECT count(*) FROM purchases p
		WHERE p.total <> (SELECT COALESCE(sum(i.line_total), 0) FROM purchase_items i WHERE i.purchase_id = p.id)
		   OR NOT EXISTS (SELECT 1 FROM purchase_items i WHERE i.purchase_id = p.id)`),
		"every purchase has items and its total is the sum of their line totals")
	require.Zero(t, f.count(t, `SELECT count(*) FROM purchase_items WHERE line_total <> unit_price * quantity`),
		"every line total is the unit price times the quantity")
}

func (f fixture) requireAttemptsMatchThePurchases(t *testing.T) {
	t.Helper()
	f.pay.mu.Lock()
	attempts := append([]payment.ChargeRequest(nil), f.pay.attempts...)
	f.pay.mu.Unlock()
	require.NotEmpty(t, attempts)
	for _, a := range attempts {
		var total, token, currency string
		require.NoError(t, f.pool.QueryRow(context.Background(),
			`SELECT total::text, payment_token, currency FROM purchases WHERE id = $1::uuid`, a.Reference).
			Scan(&total, &token, &currency))
		require.Equal(t, total, a.Amount.StringFixed(2), "the amount charged for %s", a.Reference)
		require.Equal(t, token, a.Token)
		require.Equal(t, currency, a.Currency)
	}
}

func (f fixture) fillCart(t *testing.T, user string, quantities map[string]int) {
	t.Helper()
	for id, qty := range quantities {
		require.NoError(t, f.orders.AddToCart(context.Background(), user, id, qty))
	}
}

func requireSameSnapshot(t *testing.T, want, got ordering.Purchase) {
	t.Helper()
	require.Equal(t, want.ID, got.ID)
	require.Equal(t, want.Status, got.Status)
	require.Equal(t, want.PaymentRef, got.PaymentRef)
	require.Equal(t, want.FailureReason, got.FailureReason)
	require.Equal(t, want.Currency, got.Currency)
	require.Equal(t, want.Total.StringFixed(2), got.Total.StringFixed(2))
	require.Len(t, got.Items, len(want.Items))
	for i := range want.Items {
		w, g := want.Items[i], got.Items[i]
		require.Equal(t, []any{w.ProductID, w.SKU, w.Name, w.Quantity}, []any{g.ProductID, g.SKU, g.Name, g.Quantity})
		require.Equal(t, w.UnitPrice.StringFixed(2), g.UnitPrice.StringFixed(2))
		require.Equal(t, w.LineTotal.StringFixed(2), g.LineTotal.StringFixed(2))
	}
}

func TestEveryChargeAttemptMatchesThePersistedPurchase(t *testing.T) {
	f := setup(t, newProvider(approve))
	ctx := context.Background()
	a, b := f.product(t, "A-1", "29.99", 20), f.product(t, "B-1", "0.10", 20)
	cart := map[string]int{a.ID: 2, b.ID: 3}

	f.fillCart(t, "alice", cart)
	approved, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.NoError(t, err)
	require.Equal(t, ordering.StatusConfirmed, approved.Purchase.Status)
	require.Equal(t, "60.28", approved.Purchase.Total.StringFixed(2))
	require.Len(t, f.pay.attemptsFor(approved.Purchase.ID), 1)

	f.fillCart(t, "alice", cart)
	replayed, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.NoError(t, err)
	require.Equal(t, ordering.Replayed, replayed.Kind)
	requireSameSnapshot(t, approved.Purchase, replayed.Purchase)
	require.Len(t, f.pay.attemptsFor(approved.Purchase.ID), 1, "a repetition does not charge again")

	declined, err := f.orders.CheckoutCart(ctx, "alice", otherKey, "pm_declined")
	require.NoError(t, err)
	require.Equal(t, ordering.StatusFailed, declined.Purchase.Status)
	again, err := f.orders.CheckoutCart(ctx, "alice", otherKey, "pm_declined")
	require.NoError(t, err)
	require.Equal(t, ordering.Replayed, again.Kind)
	requireSameSnapshot(t, declined.Purchase, again.Purchase)
	require.Len(t, f.pay.attemptsFor(declined.Purchase.ID), 1)

	f.pay.set(unknownAfterCharge)
	pending, err := f.orders.CheckoutCart(ctx, "alice", thirdKey, "pm_approved")
	require.NoError(t, err)
	require.Equal(t, ordering.StatusPending, pending.Purchase.Status)
	f.pay.set(unavailable)
	report, err := f.orders.RecoverPending(ctx)
	require.NoError(t, err)
	require.Len(t, report.StillPending, 1)
	f.pay.set(approve)
	report, err = f.orders.RecoverPending(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, report.Confirmed)

	attempts := f.pay.attemptsFor(pending.Purchase.ID)
	require.Len(t, attempts, 3)
	for _, at := range attempts[1:] {
		require.Equal(t, attempts[0], at, "every retry sends the same reference, token, amount and currency")
	}
	recovered, err := f.orders.Purchase(ctx, "alice", pending.Purchase.ID)
	require.NoError(t, err)
	require.Equal(t, ordering.StatusConfirmed, recovered.Status)
	recovered.Status = pending.Purchase.Status
	recovered.PaymentRef = pending.Purchase.PaymentRef
	requireSameSnapshot(t, pending.Purchase, recovered)

	require.Equal(t, 3, f.count(t, `SELECT count(*) FROM purchases`))
	require.Equal(t, 3, f.pay.effective(), "one effective charge per purchase")
	f.requireAttemptsMatchThePurchases(t)
	f.requireMoneyInvariants(t)
}
