package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

const stuckAfter = 3

func policy(owner string) app.RecoveryPolicy {
	return app.RecoveryPolicy{
		Owner: owner, MinAge: time.Minute, Lease: time.Minute, MaxPendingAge: time.Hour, Batch: 10, StuckAfter: stuckAfter,
		Backoff: func(int) time.Duration { return time.Second },
	}
}

func (f *fixture) recovery(t *testing.T) *app.Recovery {
	t.Helper()
	r, err := app.NewRecovery(f.w, f.w, f.pay, f.w, f.w, f.rec, budgets, policy("worker-a"))
	require.NoError(t, err)
	return r
}

func (f *fixture) pending(t *testing.T) domain.Purchase {
	t.Helper()
	f.pay.set(unknownAfterCharge)
	res, err := f.run(t, f.standard())
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, res.Purchase.Status)
	return res.Purchase
}

func (f *fixture) status(t *testing.T, id domain.ID) domain.Status {
	t.Helper()
	p, err := f.w.Get(context.Background(), "alice", id)
	require.NoError(t, err)
	return p.Status
}

func TestRecoveryConfirmsAnUnknownChargeWithoutChargingTwice(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	f.w.state.carts["alice"] = []domain.CartItem{{ProductID: f.a, Quantity: 2, UnitPriceSeen: *price("29.99")}}
	p := f.pending(t)
	require.Equal(t, 1, f.pay.effective())

	f.pay.set(approve)
	report, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RecoveryReport{Confirmed: 1}, report)
	require.Equal(t, 1, f.pay.effective(), "the retried charge reuses the original one")
	require.Equal(t, f.pay.requests[0], f.pay.requests[1], "same reference, token, amount and currency")
	require.Equal(t, domain.StatusConfirmed, f.status(t, p.ID))
	require.Empty(t, f.w.state.carts["alice"])
	require.Equal(t, []string{app.RecoveryConfirmed}, f.rec.attemptOutcomes())

	report, err = f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RecoveryReport{}, report, "running it again changes nothing")
	require.Equal(t, 2, f.pay.received())
}

func TestRecoveryReschedulesWhileTheProviderIsUnavailable(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	p := f.pending(t)

	f.pay.set(unavailable)
	report, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Len(t, report.StillPending, 1)
	require.ErrorIs(t, report.StillPending[0].Cause, app.ErrPaymentUnavailable)
	require.Equal(t, domain.StatusPending, f.status(t, p.ID))
	require.Equal(t, 1, f.w.state.attempts[p.ID])
	require.Empty(t, f.w.state.owners[p.ID], "the lease is released with the backoff")
	require.Zero(t, f.w.state.releases, "an unavailable provider does not prove the earlier attempt did not charge")
	require.Equal(t, []app.RecoveryAttempt{{PurchaseID: p.ID, Attempts: 1, Outcome: app.RecoveryStillPending,
		Cause: report.StillPending[0].Cause, StuckAfter: stuckAfter}}, f.rec.attempts)
	require.Equal(t, app.PendingStats{Pending: 1}, f.rec.gauges[0])
}

func TestRecoveryDeclinesAndReleasesTheReservationOnce(t *testing.T) {
	f := setup(t, decline)
	f.pay.set(unknownBeforeCharge)
	res, err := f.run(t, f.standard())
	require.NoError(t, err)
	f.pay.set(decline)

	report, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RecoveryReport{Declined: 1}, report)
	require.Equal(t, domain.StatusFailed, f.status(t, res.Purchase.ID))
	require.Equal(t, 1, f.w.state.releases)
}

func TestAFailedFinalizationIsAnErrorAndIsRetried(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	p := f.pending(t)
	f.pay.set(approve)
	f.w.finErr = errors.New("connection reset")

	report, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Len(t, report.StillPending, 1)
	require.Equal(t, []string{app.RecoveryError}, f.rec.attemptOutcomes())
	require.Equal(t, 1, f.w.state.attempts[p.ID])
}

func TestAPurchaseTakenByAnotherWorkerIsSkippedWithoutCharging(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	p := f.pending(t)
	received := f.pay.received()
	f.pay.set(approve)
	f.w.onRenew = func(id domain.ID) {
		f.w.mu.Lock()
		defer f.w.mu.Unlock()
		f.w.state.owners[id] = "worker-b"
	}

	report, err := f.recovery(t).RunCycle(context.Background())

	require.NoError(t, err)
	require.Equal(t, app.RecoveryReport{Skipped: 1}, report)
	require.Equal(t, received, f.pay.received(), "a purchase whose lease was taken is not charged")
	require.Equal(t, []string{app.RecoverySkipped}, f.rec.attemptOutcomes())
	require.Equal(t, domain.StatusPending, f.status(t, p.ID))
	require.Zero(t, f.w.state.attempts[p.ID], "the other worker's attempt is not counted twice")
}

func TestAStuckPurchaseIsResolvedWithTheProvider(t *testing.T) {
	tests := map[string]struct {
		charged    bool
		approve    bool
		amount     string
		status     domain.Status
		result     string
		releases   int
		reason     string
		stillOpen  bool
		voided     int
		resolution string
	}{
		"approved with the same amount": {charged: true, approve: true, status: domain.StatusConfirmed, result: app.ResolutionConfirmed},
		"declined by the provider":      {charged: true, status: domain.StatusFailed, result: app.ResolutionDeclined, releases: 1, reason: "card_declined"},
		"never charged, so voided": {status: domain.StatusFailed, result: app.ResolutionVoided, releases: 1,
			reason: app.ReasonPaymentNotCompleted, voided: 1},
		"approved with another amount": {charged: true, approve: true, amount: "1.00", status: domain.StatusPending, result: app.ResolutionMismatch, stillOpen: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := setup(t, unknownBeforeCharge)
			res, err := f.run(t, f.standard())
			require.NoError(t, err)
			p := res.Purchase
			if tc.charged {
				c := app.Charge{Reference: string(p.ID), Token: p.PaymentToken, Amount: p.Total, Currency: p.Currency}
				if tc.amount != "" {
					c.Amount = decimal.RequireFromString(tc.amount)
				}
				f.pay.charges[string(p.ID)] = c
				f.pay.results[string(p.ID)] = app.ChargeResult{Approved: tc.approve, ProviderRef: "ref_x", Reason: map[bool]string{false: "card_declined"}[tc.approve]}
			}
			f.w.state.attempts[p.ID] = stuckAfter
			received := f.pay.received()

			report, err := f.recovery(t).RunCycle(context.Background())
			require.NoError(t, err)
			require.Equal(t, received, f.pay.received(), "a stuck purchase is never charged again")
			require.Equal(t, []string{tc.result}, f.rec.resolutionResults())
			require.Equal(t, tc.voided, report.Voided)
			got, err := f.w.Get(context.Background(), "alice", p.ID)
			require.NoError(t, err)
			require.Equal(t, tc.status, got.Status)
			require.Equal(t, tc.reason, got.FailureReason)
			require.Equal(t, tc.releases, f.w.state.releases)
			if tc.stillOpen {
				require.Equal(t, stuckAfter+1, f.w.state.attempts[p.ID])
			}
		})
	}
}

func TestAnUnansweredLookupKeepsTheStuckPurchasePending(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	p := f.pending(t)
	f.w.state.attempts[p.ID] = stuckAfter
	f.pay.set(unavailable)

	report, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Len(t, report.StillPending, 1)
	require.Equal(t, []string{app.ResolutionError}, f.rec.resolutionResults())
	require.Equal(t, domain.StatusPending, f.status(t, p.ID))
	require.Zero(t, f.pay.voids, "nothing is voided without a lookup")
}

func TestAnOverduePurchaseIsResolvedBelowTheThreshold(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	p := f.pending(t)
	f.w.overdue = map[domain.ID]bool{p.ID: true}
	received := f.pay.received()

	_, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, received, f.pay.received())
	require.Equal(t, 1, f.pay.lookups)
	require.Equal(t, []string{app.ResolutionConfirmed}, f.rec.resolutionResults(), "the lost answer was an approval")
	require.Equal(t, domain.StatusConfirmed, f.status(t, p.ID))
}

func TestAYoungPurchaseBelowTheThresholdIsNeverVoided(t *testing.T) {
	f := setup(t, unknownBeforeCharge)
	_, err := f.run(t, f.standard())
	require.NoError(t, err)

	_, err = f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Zero(t, f.pay.voids)
	require.Zero(t, f.pay.lookups)
}

func TestTheGaugesCountStuckPurchasesAndTheirUnits(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	p := f.pending(t)
	f.pay.set(unavailable)
	f.w.state.attempts[p.ID] = stuckAfter

	_, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.PendingStats{Pending: 1, Stuck: 1, StuckReservedUnits: 3}, f.rec.gauges[0])
}

func TestRecoveryFailsWhenThePendingPurchasesCannotBeClaimed(t *testing.T) {
	f := setup(t, approve)
	f.w.listErr = errors.New("connection refused")
	_, err := f.recovery(t).RunCycle(context.Background())
	require.ErrorContains(t, err, "connection refused")
}

func TestNewRecoveryValidatesItsDependencies(t *testing.T) {
	f := setup(t, approve)
	_, err := app.NewRecovery(nil, f.w, f.pay, f.w, f.w, f.rec, budgets, policy("a"))
	require.Error(t, err)
	_, err = app.NewRecovery(f.w, f.w, f.pay, f.w, f.w, f.rec, app.Budgets{}, policy("a"))
	require.Error(t, err)
	for name, change := range map[string]func(p *app.RecoveryPolicy){
		"owner":   func(p *app.RecoveryPolicy) { p.Owner = "" },
		"backoff": func(p *app.RecoveryPolicy) { p.Backoff = nil },
		"lease":   func(p *app.RecoveryPolicy) { p.Lease = 0 },
		"batch":   func(p *app.RecoveryPolicy) { p.Batch = 0 },
		"stuck":   func(p *app.RecoveryPolicy) { p.StuckAfter = 0 },
	} {
		p := policy("a")
		change(&p)
		_, err := app.NewRecovery(f.w, f.w, f.pay, f.w, f.w, f.rec, budgets, p)
		require.Error(t, err, name)
	}
}
