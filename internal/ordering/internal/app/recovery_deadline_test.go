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
