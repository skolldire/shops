package app_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

func TestAFinishedPurchaseRecordsOneOutcomeEvent(t *testing.T) {
	for name, tc := range map[string]struct {
		mode   mode
		kind   domain.EventType
		reason string
	}{
		"approved": {approve, domain.EventPurchaseConfirmed, ""},
		"declined": {decline, domain.EventPurchaseFailed, "card_declined"},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, tc.mode)
			res, err := f.run(t, f.standard())
			require.NoError(t, err)
			require.Len(t, f.w.state.events, 1)
			e := f.w.state.events[0]
			require.Equal(t, tc.kind, e.Type)
			require.Equal(t, res.Purchase.ID, e.AggregateID)
			require.Equal(t, "purchase:"+string(res.Purchase.ID), e.CorrelationID)
			require.Equal(t, domain.PurchaseEventVersion, e.Version)
			require.Equal(t, tc.reason, e.Reason)
			require.Len(t, e.Items, 2)
			require.Equal(t, 2, e.Items[0].Quantity)
			printed := fmt.Sprintf("%+v", e)
			require.NotContains(t, printed, "pm_approved", "no payment token")
			require.NotContains(t, printed, "alice", "no user")
			require.NotContains(t, printed, key, "no idempotency key")
		})
	}
}

func TestAFinalizationThatLosesFinishRecordsNoEvent(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	p := f.pending(t)
	f.pay.set(approve)
	f.w.onRenew = func(id domain.ID) {
		f.w.mu.Lock()
		defer f.w.mu.Unlock()
		settled := f.w.state.purchases[id]
		settled.Status = domain.StatusConfirmed
		f.w.state.purchases[id] = settled
		f.w.state.owners[id] = "worker-a"
	}
	appends := f.w.appends

	_, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, appends, f.w.appends, "another actor finished the purchase, so this finalization never tries to append")
	require.Empty(t, f.w.state.events)
	require.Equal(t, domain.StatusConfirmed, f.status(t, p.ID))
}

func TestAFailedAppendRollsTheFinalizationBack(t *testing.T) {
	f := setup(t, approve)
	f.w.state.carts["alice"] = []domain.CartItem{{ProductID: f.a, Quantity: 2, UnitPriceSeen: *price("29.99")}}
	f.w.appendErr = errors.New("duplicate key value violates unique constraint")

	res, err := f.run(t, f.standard())
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, res.Purchase.Status, "the purchase stays pending")
	require.Equal(t, domain.StatusPending, f.status(t, res.Purchase.ID))
	require.Zero(t, f.w.state.commits, "the sale was rolled back")
	require.Equal(t, 2, f.w.state.reserved[f.a], "the reservation is kept for the recovery")
	require.Len(t, f.w.state.carts["alice"], 1, "the cart is untouched")
	require.Empty(t, f.w.state.events)
}

func TestTheSettlementRequiresTheOutbox(t *testing.T) {
	w := newWorld()
	ports := checkoutPorts(w, newProvider(approve), &recorder{})
	ports.Events = nil
	_, err := app.NewCheckout(ports, budgets)
	require.Error(t, err)
	_, err = domain.NewPurchaseEvent(domain.Purchase{ID: "p"}, domain.Outcome{Status: domain.StatusPending})
	require.ErrorIs(t, err, domain.ErrNoEventForOutcome)
}
