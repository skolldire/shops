package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

func (f *fixture) recovery(t *testing.T, total time.Duration) *app.Recovery {
	t.Helper()
	r, err := app.NewRecovery(f.w, f.w, f.pay, f.w, f.w, f.rec, budgets, total)
	require.NoError(t, err)
	return r
}

func TestRecoveryConfirmsAnUnknownChargeWithoutChargingTwice(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	f.w.state.carts["alice"] = []domain.CartItem{{ProductID: f.a, Quantity: 2, UnitPriceSeen: *price("29.99")}}
	first, err := f.run(t, f.standard())
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, first.Purchase.Status)
	require.Equal(t, 1, f.pay.effective())

	f.pay.set(approve)
	report, err := f.recovery(t, time.Minute).RecoverPending(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RecoveryReport{Confirmed: 1}, report)
	require.Equal(t, 1, f.pay.effective(), "the retried charge reuses the original one")
	require.Equal(t, 2, f.pay.received())
	require.Equal(t, f.pay.requests[0], f.pay.requests[1], "same reference, token, amount and currency")

	p, err := f.w.Get(context.Background(), "alice", first.Purchase.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusConfirmed, p.Status)
	require.Equal(t, 3, f.w.stockOf(f.a))
	require.Empty(t, f.w.state.carts["alice"])
	require.Equal(t, []string{app.RecoveryConfirmed}, f.rec.recovery)

	report, err = f.recovery(t, time.Minute).RecoverPending(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RecoveryReport{}, report, "running it again changes nothing")
	require.Equal(t, 2, f.pay.received())
}

func TestRecoveryKeepsPendingWhileTheProviderIsUnavailable(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	first, err := f.run(t, f.standard())
	require.NoError(t, err)

	f.pay.set(unavailable)
	report, err := f.recovery(t, time.Minute).RecoverPending(context.Background())
	require.NoError(t, err)
	require.Zero(t, report.Confirmed)
	require.Len(t, report.StillPending, 1)
	require.Equal(t, first.Purchase.ID, report.StillPending[0].PurchaseID)
	require.ErrorIs(t, report.StillPending[0].Cause, app.ErrPaymentUnavailable)
	require.Zero(t, f.w.state.returns, "an unavailable provider does not prove the earlier attempt did not charge")
	require.Equal(t, 3, f.w.stockOf(f.a))

	f.pay.set(approve)
	report, err = f.recovery(t, time.Minute).RecoverPending(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.Confirmed)
	require.Equal(t, 1, f.pay.effective())
	require.Equal(t, []string{app.RecoveryStillPending, app.RecoveryConfirmed}, f.rec.recovery)
}

func TestRecoveryDeclinesAndReturnsStockOnce(t *testing.T) {
	f := setup(t, unknownBeforeCharge)
	_, err := f.run(t, order(key, "pm_declined", line(f.a, 2, "29.99")))
	require.NoError(t, err)
	f.pay.set(approve)
	report, err := f.recovery(t, time.Minute).RecoverPending(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RecoveryReport{Declined: 1}, report)
	require.Equal(t, 5, f.w.stockOf(f.a))
	require.Equal(t, 1, f.w.state.returns)
}

func TestRecoveryRespectsItsTotalBudgetDuringACharge(t *testing.T) {
	f := setup(t, unknownBeforeCharge)
	_, err := f.run(t, f.standard())
	require.NoError(t, err)
	_, err = f.run(t, order(otherKey, "pm_approved", line(f.a, 1, "29.99")))
	require.NoError(t, err)

	f.pay.set(blockUntilDone)
	start := time.Now()
	report, err := f.recovery(t, 50*time.Millisecond).RecoverPending(context.Background())
	require.NoError(t, err)
	require.Less(t, time.Since(start), 500*time.Millisecond, "the total budget cuts the charge in flight")
	require.Len(t, report.StillPending, 2)
	require.ErrorIs(t, report.StillPending[0].Cause, context.DeadlineExceeded)
	require.ErrorIs(t, report.StillPending[1].Cause, context.DeadlineExceeded)
}

func TestRecoveryKeepsPendingWhenTheFinalizationFails(t *testing.T) {
	f := setup(t, unknownBeforeCharge)
	_, err := f.run(t, f.standard())
	require.NoError(t, err)
	f.pay.set(approve)
	f.w.finErr = errors.New("connection reset")
	report, err := f.recovery(t, time.Minute).RecoverPending(context.Background())
	require.NoError(t, err)
	require.Len(t, report.StillPending, 1)
	require.ErrorContains(t, report.StillPending[0].Cause, "connection reset")
}

func TestRecoveryFailsWhenThePendingPurchasesCannotBeRead(t *testing.T) {
	f := setup(t, approve)
	f.w.listErr = errors.New("database is down")
	_, err := f.recovery(t, time.Minute).RecoverPending(context.Background())
	require.ErrorContains(t, err, "database is down")
}

func TestNewRecoveryValidatesItsDependencies(t *testing.T) {
	w := newWorld()
	_, err := app.NewRecovery(w, w, newProvider(approve), w, w, &recorder{}, budgets, 0)
	require.Error(t, err)
	_, err = app.NewRecovery(w, nil, newProvider(approve), w, w, &recorder{}, budgets, time.Minute)
	require.Error(t, err)
}
