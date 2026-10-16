package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type call struct {
	op       string
	id       domain.ID
	deadline time.Duration
}

type controls struct {
	mu    sync.Mutex
	calls []call
	fail  map[string]domain.ID
}

func (c *controls) hook(ctx context.Context, op string, id domain.ID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	got := call{op: op, id: id, deadline: -1}
	if deadline, ok := ctx.Deadline(); ok {
		got.deadline = time.Until(deadline)
	}
	c.calls = append(c.calls, got)
	if failing, ok := c.fail[op]; ok && failing == id {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *controls) ops() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.calls))
	for i, got := range c.calls {
		out[i] = got.op
	}
	return out
}

func (f *fixture) controlled(fail map[string]domain.ID) *controls {
	c := &controls{fail: fail}
	f.w.control = c.hook
	return c
}

func (f *fixture) twoPending(t *testing.T) (domain.Purchase, domain.Purchase) {
	t.Helper()
	first := f.pending(t)
	res, err := f.run(t, order(otherKey, "pm_approved", line(f.b, 1, "5.00")))
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, res.Purchase.Status)
	return first, res.Purchase
}

func TestEveryControlQueryRunsWithinTheDatabaseTimeout(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	f.pending(t)
	f.pay.set(unavailable)
	c := f.controlled(nil)

	_, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"claim", "renew", "reschedule", "stats"}, c.ops())
	for _, got := range c.calls {
		require.Positive(t, got.deadline, got.op)
		require.LessOrEqual(t, got.deadline, time.Second, got.op)
	}
}

func TestARenewalThatTimesOutChargesNothingAndTheNextPurchaseContinues(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	first, second := f.twoPending(t)
	f.pay.set(approve)
	received := f.pay.received()
	f.controlled(map[string]domain.ID{"renew": first.ID})

	report, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.Confirmed)
	require.Len(t, report.StillPending, 1)
	require.Equal(t, first.ID, report.StillPending[0].PurchaseID)
	require.ErrorIs(t, report.StillPending[0].Cause, context.DeadlineExceeded)
	require.Equal(t, received+1, f.pay.received(), "only the second purchase was charged")
	require.Equal(t, domain.StatusPending, f.status(t, first.ID))
	require.Equal(t, domain.StatusConfirmed, f.status(t, second.ID))
	require.Equal(t, []string{app.RecoveryError, app.RecoveryConfirmed}, f.rec.attemptOutcomes())
}

func TestARescheduleThatTimesOutLeavesTheLeaseToExpire(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	p := f.pending(t)
	f.pay.set(unavailable)
	f.controlled(map[string]domain.ID{"reschedule": p.ID})

	report, err := f.recovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Len(t, report.StillPending, 1)
	require.Equal(t, "worker-a", f.w.state.owners[p.ID], "the lease stays until it expires")
	require.Zero(t, f.w.state.attempts[p.ID])
	require.Len(t, f.rec.attempts, 1)
	require.Equal(t, app.RecoveryError, f.rec.attempts[0].Outcome)
	require.ErrorIs(t, f.rec.attempts[0].Cause, context.DeadlineExceeded)
}

func TestGaugesThatTimeOutKeepTheirLastValues(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	f.pending(t)
	f.controlled(map[string]domain.ID{"stats": ""})

	_, err := f.recovery(t).RunCycle(context.Background())
	require.ErrorContains(t, err, "gauges were not updated")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, f.rec.gauges, "no gauge is overwritten")
}

func TestStoppingBetweenPurchasesStartsNoOther(t *testing.T) {
	f := setup(t, unknownAfterCharge)
	first, second := f.twoPending(t)
	f.pay.set(approve)
	ctx, cancel := context.WithCancel(context.Background())
	f.pay.onCharge = cancel
	c := f.controlled(nil)

	report, err := f.recovery(t).RunCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, app.RecoveryReport{Confirmed: 1}, report, "the purchase in progress finishes")
	require.Equal(t, domain.StatusConfirmed, f.status(t, first.ID))
	require.Equal(t, domain.StatusPending, f.status(t, second.ID))
	require.Equal(t, []string{"claim", "renew"}, c.ops(), "neither the second purchase nor the gauges are read after stopping")
}

func tight(owner string) app.RecoveryPolicy {
	p := policy(owner)
	p.DBTimeout = 50 * time.Millisecond
	return p
}

var tightBudgets = app.Budgets{Reserve: time.Second, Charge: 200 * time.Millisecond, Finalize: 50 * time.Millisecond}

func (f *fixture) tightRecovery(t *testing.T) *app.Recovery {
	t.Helper()
	r, err := app.NewRecovery(recoveryPorts(f.w, f.pay, f.rec), tightBudgets, tight("worker-a"))
	require.NoError(t, err)
	return r
}

func (f *fixture) stuck(t *testing.T) domain.Purchase {
	t.Helper()
	f.pay.set(unknownBeforeCharge)
	res, err := f.run(t, f.standard())
	require.NoError(t, err)
	f.w.state.attempts[res.Purchase.ID] = stuckAfter
	return res.Purchase
}

func TestTheItemBudgetCoversTheRenewalTheLongestPathAndTheReschedule(t *testing.T) {
	require.Equal(t, 19*time.Second, app.ItemBudget(app.Budgets{Charge: 6 * time.Second, Finalize: 3 * time.Second}, 2*time.Second),
		"2s renewal + 6s lookup + 6s void + 3s finalization + 2s reschedule")
	_, err := app.NewRecovery(recoveryPorts(newWorld(), newProvider(approve), &recorder{}),
		app.Budgets{Reserve: time.Second, Charge: 6 * time.Second, Finalize: 3 * time.Second},
		app.RecoveryPolicy{Owner: "a", MinAge: time.Minute, Lease: 12 * time.Second, MaxPendingAge: time.Hour, DBTimeout: 2 * time.Second,
			Batch: 1, StuckAfter: 1, Backoff: func(int) time.Duration { return time.Second }})
	require.ErrorContains(t, err, "lease must exceed the deadline of one purchase plus the margin")
}

func TestAProviderThatOverrunsLeavesTheNextStepOnlyTheTimeLeft(t *testing.T) {
	f := setup(t, unknownBeforeCharge)
	p := f.stuck(t)
	f.pay.set(approve)
	var renewedAt time.Time
	f.w.control = func(_ context.Context, op string, _ domain.ID) error {
		if op == "renew" {
			renewedAt = time.Now()
		}
		return nil
	}
	f.pay.onLookup = func() { <-time.After(400 * time.Millisecond) }

	_, err := f.tightRecovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	item := app.ItemBudget(tightBudgets, 50*time.Millisecond)
	void, ok := f.pay.deadlines["void"]
	require.True(t, ok, "the void still had time")
	require.False(t, void.After(renewedAt.Add(item)), "the void ends within the purchase deadline counted from before the renewal")
	require.Less(t, void.Sub(renewedAt), 400*time.Millisecond+tightBudgets.Charge, "it did not get a full budget after the slow lookup")
	require.Equal(t, domain.StatusFailed, f.status(t, p.ID))
}

func TestAVoidWithoutTimeLeftChangesNothingAndALaterCycleSettlesOnce(t *testing.T) {
	f := setup(t, unknownBeforeCharge)
	p := f.stuck(t)
	f.pay.set(approve)
	item := app.ItemBudget(tightBudgets, 50*time.Millisecond)
	f.pay.onLookup = func() { <-time.After(item + 50*time.Millisecond) }

	report, err := f.tightRecovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Len(t, report.StillPending, 1)
	require.ErrorContains(t, report.StillPending[0].Cause, "used up its recovery deadline")
	require.Zero(t, f.pay.voids, "no financial operation starts after the deadline")
	require.Equal(t, domain.StatusPending, f.status(t, p.ID))
	require.Zero(t, f.w.state.releases)
	require.Equal(t, "worker-a", f.w.state.owners[p.ID], "no time is left to reschedule, so the lease expires on its own")
	require.Equal(t, []string{app.ResolutionError}, f.rec.resolutionResults())

	f.pay.onLookup = nil
	delete(f.w.state.owners, p.ID)
	report, err = f.tightRecovery(t).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.Voided)
	require.Equal(t, domain.StatusFailed, f.status(t, p.ID))
	require.Equal(t, 1, f.w.state.releases, "a single release")
}

type checkoutOnly struct{}

func (checkoutOnly) Checkout(context.Context, string, *domain.Purchase, error) {}

type recoveryOnly struct{}

func (recoveryOnly) RecoveryAttempt(context.Context, app.RecoveryAttempt)       {}
func (recoveryOnly) RecoveryResolution(context.Context, app.RecoveryResolution) {}
func (recoveryOnly) RecoveryGauges(context.Context, app.PendingStats)           {}

func TestEachUseCaseTakesOnlyTheRecorderItUses(t *testing.T) {
	w := newWorld()
	_, err := app.NewCheckout(checkoutPorts(w, newProvider(approve), checkoutOnly{}), budgets)
	require.NoError(t, err, "a checkout records checkouts only")
	_, err = app.NewRecovery(recoveryPorts(w, newProvider(approve), recoveryOnly{}), budgets, policy("a"))
	require.NoError(t, err, "a recovery records attempts, resolutions and gauges only")
}
