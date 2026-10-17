//go:build integration

package ordering_test

import (
	"context"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/logger"
)

const dbTimeout = 300 * time.Millisecond

func tightCluster(t *testing.T) cluster {
	t.Helper()
	return newClusterWith(t, 10, func(c *ordering.RecoveryConfig) { c.DBTimeout = dbTimeout })
}

func (c cluster) pendingSorted(t *testing.T, n int) []string {
	t.Helper()
	c.pay.set(false, true)
	ids := make([]string, n)
	for i := range n {
		ids[i] = c.buy(t, []string{"alice", "bob"}[i%2], c.product(t, string(rune('A'+i))+"-1", 5), "pm_approved").ID
	}
	sort.Strings(ids)
	c.age(t, "1 minute")
	c.pay.set(false, false)
	return ids
}

func (c cluster) hold(t *testing.T, sql string, args ...any) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := c.pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, sql, args...)
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback(ctx) }) }
	t.Cleanup(release)
	return release
}

func (c cluster) waitingOn(t *testing.T, query string) bool {
	t.Helper()
	return c.count(t, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE $1`, query+"%") > 0
}

func (c cluster) lockWaitAtLeast(t *testing.T, query string, d time.Duration) bool {
	t.Helper()
	return c.count(t, `SELECT count(*) FROM pg_stat_activity
		WHERE wait_event_type = 'Lock' AND query LIKE $1 AND now() - query_start >= $2::interval`, query+"%", d.String()) > 0
}

func (c cluster) leaseOwned(t *testing.T, id string) bool {
	t.Helper()
	return c.count(t, `SELECT count(*) FROM purchases WHERE id = $1::uuid AND lease_owner IS NOT NULL`, id) == 1
}

func done(t *testing.T, result <-chan recovered, within time.Duration) recovered {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(within):
		t.Fatal("the recovery cycle did not finish within its deadlines")
		return recovered{}
	}
}

func TestABlockedRenewalTimesOutChargesNothingAndTheNextPurchaseContinues(t *testing.T) {
	c := tightCluster(t)
	ids := c.pendingSorted(t, 3)
	var release func()
	c.pay.onCharge = func(reference string) {
		if reference == ids[0] {
			release = c.hold(t, `SELECT 1 FROM purchases WHERE id = $1::uuid FOR UPDATE`, ids[1])
		}
	}
	before := c.pay.chargesFor(ids[1])

	result := recoverAsync(context.Background(), c.a)
	require.Eventually(t, func() bool { return c.waitingOn(t, "UPDATE purchases SET lease_until") }, 10*time.Second, 5*time.Millisecond,
		"the renewal of the second purchase waits for the row lock")
	got := done(t, result, 10*time.Second)
	release()

	require.NoError(t, got.err)
	require.Equal(t, 2, got.report.Confirmed, "the first and the third purchase")
	require.Len(t, got.report.StillPending, 1)
	require.Equal(t, ids[1], got.report.StillPending[0].ID)
	require.ErrorIs(t, got.report.StillPending[0].Cause, context.DeadlineExceeded)
	require.Equal(t, before, c.pay.chargesFor(ids[1]), "a purchase whose renewal timed out is not charged")
	status, _ := c.status(t, ids[1])
	require.Equal(t, "PENDING", status)
	require.True(t, c.leaseOwned(t, ids[1]), "the lease of the claim expires on its own")
	require.Equal(t, 1, c.a.logs.count("INFO", "renew the recovery lease"))
}

func TestASlowRenewalIsCountedInsideThePurchaseDeadline(t *testing.T) {
	c := tightCluster(t)
	ids := c.pendingSorted(t, 2)
	var release func()
	c.pay.onCharge = func(reference string) {
		if reference == ids[0] {
			release = c.hold(t, `SELECT 1 FROM purchases WHERE id = $1::uuid FOR UPDATE`, ids[1])
		}
	}

	started := time.Now()
	result := recoverAsync(context.Background(), c.a)
	require.Eventually(t, func() bool { return c.lockWaitAtLeast(t, "UPDATE purchases SET lease_until", dbTimeout/2) }, 10*time.Second, 5*time.Millisecond)
	release()
	got := done(t, result, 10*time.Second)

	require.NoError(t, got.err)
	require.Equal(t, 2, got.report.Confirmed, "the renewal finished within db_timeout")
	item := ordering.RecoveryItemBudget(ordering.Budgets{Reserve: 3 * time.Second, Charge: 3 * time.Second, Finalize: 3 * time.Second}, dbTimeout)
	deadline := c.pay.deadlines[ids[1]]
	require.False(t, deadline.IsZero())
	require.False(t, deadline.After(started.Add(item)), "the charge ends within the deadline counted from before the renewal")
}

func TestABlockedRescheduleTimesOutAndLeavesTheLeaseToExpire(t *testing.T) {
	c := tightCluster(t)
	ids := c.pendingSorted(t, 1)
	var release func()
	c.pay.onCharge = func(string) {
		release = c.hold(t, `SELECT 1 FROM purchases WHERE id = $1::uuid FOR UPDATE`, ids[0])
		c.pay.failing = ids[0]
	}

	result := recoverAsync(context.Background(), c.a)
	require.Eventually(t, func() bool { return c.waitingOn(t, "UPDATE purchases") }, 10*time.Second, 5*time.Millisecond,
		"the reschedule waits for the row lock")
	got := done(t, result, 10*time.Second)
	release()

	require.NoError(t, got.err)
	require.Len(t, got.report.StillPending, 1)
	require.True(t, c.leaseOwned(t, ids[0]))
	require.Zero(t, c.count(t, `SELECT recovery_attempts FROM purchases WHERE id = $1::uuid`, ids[0]), "the attempt was not recorded")
	require.Equal(t, 1, c.a.logs.count("INFO", "reschedule the recovery"))
}

func TestBlockedGaugesKeepTheirLastValues(t *testing.T) {
	c := tightCluster(t)
	ids := c.pendingSorted(t, 1)
	c.pay.set(false, true)
	_, err := c.a.orders.RecoverOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, float64(1), c.a.metrics.gauge("checkout_pending_purchases"))
	c.due(t)
	c.pay.set(false, false)
	var release func()
	c.pay.onCharge = func(string) { release = c.hold(t, `LOCK TABLE purchase_items IN ACCESS EXCLUSIVE MODE`) }

	result := recoverAsync(context.Background(), c.a)
	got := done(t, result, 10*time.Second)
	release()

	require.ErrorContains(t, got.err, "gauges were not updated")
	require.ErrorIs(t, got.err, context.DeadlineExceeded)
	status, _ := c.status(t, ids[0])
	require.Equal(t, "CONFIRMED", status)
	require.Equal(t, float64(1), c.a.metrics.gauge("checkout_pending_purchases"), "the last value stays until a cycle reads a new one")
}

func TestAnExhaustedPoolEndsTheCycleWithinTheDatabaseTimeout(t *testing.T) {
	c := tightCluster(t)
	c.pendingSorted(t, 1)
	cfg := c.cfg
	cfg.MaxConns = 1
	small, err := database.Open(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(small.Close)
	quiet, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	config := recoveryConfig
	config.DBTimeout = dbTimeout
	orders, err := ordering.NewModule(ordering.Deps{
		Pool: small, Catalog: c.cat, Payments: c.pay, Log: quiet, Metrics: &spy{counters: map[string]int64{}, gauges: map[string]float64{}},
		Users: []string{"alice"}, Budgets: ordering.Budgets{Reserve: 3 * time.Second, LockTimeout: time.Second, Charge: 3 * time.Second, Finalize: 3 * time.Second},
		Recovery: config,
	})
	require.NoError(t, err)
	conn, err := small.Acquire(context.Background())
	require.NoError(t, err)
	defer conn.Release()

	started := time.Now()
	_, err = orders.RecoverOnce(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), dbTimeout+time.Second)
}

func TestStoppingTheWorkerWithItsContextFinishesThePurchaseInProgressAndStartsNoOther(t *testing.T) {
	c := tightCluster(t)
	ids := c.pendingSorted(t, 2)
	before := c.pay.chargesFor(ids[1])
	charging, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.pay.onCharge = func(string) { once.Do(func() { close(charging); <-release }) }
	ctx, stop := context.WithCancel(context.Background())
	worker := c.a.orders.RecoveryWorker()
	worker.Start(ctx)
	<-charging

	stop()
	close(release)
	expired, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, worker.Stop(expired))

	first, _ := c.status(t, ids[0])
	second, _ := c.status(t, ids[1])
	require.Equal(t, "CONFIRMED", first, "the purchase in progress finished within its deadline")
	require.Equal(t, "PENDING", second)
	require.Equal(t, before, c.pay.chargesFor(ids[1]), "the next purchase of the batch was not started")
	require.Equal(t, 1, c.a.logs.count("INFO", "recovery worker stopped"))
}
