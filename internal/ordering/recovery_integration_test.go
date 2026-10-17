//go:build integration

package ordering_test

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
	"github.com/skolldire/shops/internal/payment"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

type spy struct {
	mu       sync.Mutex
	counters map[string]int64
	gauges   map[string]float64
}

func (s *spy) Counter(_ context.Context, name string, value int64, attrs ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := name
	for _, a := range attrs {
		key += "|" + a.Value.String()
	}
	s.counters[key] += value
}

func (s *spy) Gauge(_ context.Context, name string, value float64, _ ...attribute.KeyValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gauges[name] = value
}

func (s *spy) Histogram(context.Context, string, float64, ...attribute.KeyValue) {}

func (s *spy) counter(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counters[key]
}

func (s *spy) gauge(name string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gauges[name]
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) count(severity, message string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, line := range strings.Split(b.buf.String(), "\n") {
		if strings.Contains(line, `"severity":"`+severity+`"`) && strings.Contains(line, message) {
			n++
		}
	}
	return n
}

type gate struct {
	real       *payment.Module
	mu         sync.Mutex
	down       bool
	chargeDown bool
	charges    map[string]int
	failing    string
	onCharge   func(reference string)
	deadlines  map[string]time.Time
	onLookup   func(reference string)
	beforeVoid func(reference string)
}

func (g *gate) state() (bool, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.down, g.chargeDown
}

func (g *gate) set(down, chargeDown bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.down, g.chargeDown = down, chargeDown
}

func (g *gate) Charge(ctx context.Context, r payment.ChargeRequest) (payment.ChargeResult, error) {
	g.mu.Lock()
	g.charges[r.Reference]++
	if deadline, ok := ctx.Deadline(); ok {
		g.deadlines[r.Reference] = deadline
	}
	hook := g.onCharge
	g.mu.Unlock()
	if hook != nil {
		hook(r.Reference)
	}
	g.mu.Lock()
	failing := g.failing == r.Reference
	g.mu.Unlock()
	if down, chargeDown := g.state(); down || chargeDown || failing {
		return payment.ChargeResult{}, payment.ErrUnavailable
	}
	return g.real.Charge(ctx, r)
}

func (g *gate) Lookup(ctx context.Context, reference string) (payment.Record, error) {
	if g.onLookup != nil {
		g.onLookup(reference)
	}
	if down, _ := g.state(); down {
		return payment.Record{}, payment.ErrUnavailable
	}
	return g.real.Lookup(ctx, reference)
}

func (g *gate) Void(ctx context.Context, reference string) (payment.Record, error) {
	if down, _ := g.state(); down {
		return payment.Record{}, payment.ErrUnavailable
	}
	if g.beforeVoid != nil {
		g.beforeVoid(reference)
	}
	return g.real.Void(ctx, reference)
}

func (g *gate) chargesFor(reference string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.charges[reference]
}

type replica struct {
	orders  *ordering.Module
	logs    *logBuffer
	metrics *spy
}

type cluster struct {
	cfg  database.Config
	pool *pgxpool.Pool
	cat  *catalog.Module
	pay  *gate
	a, b replica
}

func newCluster(t *testing.T, stuckAfter int) cluster {
	t.Helper()
	return newClusterWith(t, stuckAfter, func(*ordering.RecoveryConfig) {})
}

func newClusterWith(t *testing.T, stuckAfter int, tune func(*ordering.RecoveryConfig)) cluster {
	t.Helper()
	pool, cfg := dbtest.Start(t)
	quiet, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	cat, err := catalog.NewModule(catalog.Deps{Pool: pool, Log: quiet, Metrics: telemetry.Noop(), Tracer: telemetry.Noop(), Import: catalog.ImportLimits{MaxBytes: 1 << 20, MaxRows: 100}})
	require.NoError(t, err)
	real, err := payment.NewModule(pool)
	require.NoError(t, err)
	pay := &gate{real: real, charges: map[string]int{}, deadlines: map[string]time.Time{}}
	config := recoveryConfig
	config.StuckAfterAttempts = stuckAfter
	tune(&config)
	newReplica := func() replica {
		logs := &logBuffer{}
		log, err := logger.New(logger.Config{Level: "info"}, logs)
		require.NoError(t, err)
		metrics := &spy{counters: map[string]int64{}, gauges: map[string]float64{}}
		orders, err := ordering.NewModule(ordering.Deps{
			Pool: pool, Catalog: cat, Payments: pay, Log: log, Metrics: metrics, Users: []string{"alice", "bob"},
			Budgets:  ordering.Budgets{Reserve: 3 * time.Second, LockTimeout: time.Second, Charge: 3 * time.Second, Finalize: 3 * time.Second},
			Recovery: config,
			Events:   eventsConfig,
		})
		require.NoError(t, err)
		return replica{orders: orders, logs: logs, metrics: metrics}
	}
	return cluster{cfg: cfg, pool: pool, cat: cat, pay: pay, a: newReplica(), b: newReplica()}
}

func (c cluster) product(t *testing.T, sku string, stock int64) catalog.Product {
	t.Helper()
	price, weight := decimal.RequireFromString("10.00"), decimal.RequireFromString("1")
	p, err := c.cat.Create(context.Background(), catalog.CreateInput{SKU: sku, Name: sku, Category: "Tests", Price: &price, Stock: &stock, WeightKg: &weight})
	require.NoError(t, err)
	return p
}

func (c cluster) buy(t *testing.T, user string, p catalog.Product, token string) ordering.Purchase {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, c.a.orders.AddToCart(ctx, user, p.ID, 2))
	res, err := c.a.orders.CheckoutCart(ctx, user, uuid.NewString(), token)
	require.NoError(t, err)
	require.Equal(t, ordering.StatusPending, res.Purchase.Status)
	return res.Purchase
}

func (c cluster) exec(t *testing.T, replicaRole bool, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	if replicaRole {
		_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
		require.NoError(t, err)
	}
	_, err = tx.Exec(ctx, sql, args...)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
}

func (c cluster) age(t *testing.T, by string) {
	t.Helper()
	c.exec(t, true, `UPDATE purchases SET created_at = now() - $1::interval, next_attempt_at = now() WHERE status = 'PENDING'`, by)
}

func (c cluster) due(t *testing.T) {
	t.Helper()
	c.exec(t, false, `UPDATE purchases SET next_attempt_at = now() WHERE status = 'PENDING'`)
}

func (c cluster) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, c.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (c cluster) status(t *testing.T, id string) (string, string) {
	t.Helper()
	var status, reason string
	require.NoError(t, c.pool.QueryRow(context.Background(),
		`SELECT status, COALESCE(failure_reason, '') FROM purchases WHERE id = $1::uuid`, id).Scan(&status, &reason))
	return status, reason
}

type recovered struct {
	report ordering.RecoveryReport
	err    error
}

func recoverAsync(ctx context.Context, r replica) <-chan recovered {
	out := make(chan recovered, 1)
	go func() {
		report, err := r.orders.RecoverOnce(ctx)
		out <- recovered{report: report, err: err}
	}()
	return out
}

func TestEachPurchaseIsChargedUnderItsOwnRenewedLease(t *testing.T) {
	c := newCluster(t, 10)
	ctx := context.Background()
	c.pay.set(false, true)
	first, second := c.buy(t, "alice", c.product(t, "A-1", 5), "pm_approved"), c.buy(t, "bob", c.product(t, "B-1", 5), "pm_approved")
	ids := []string{first.ID, second.ID}
	sort.Strings(ids)
	c.age(t, "1 minute")
	c.pay.set(false, false)

	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.pay.onCharge = func(reference string) {
		if reference == ids[0] {
			once.Do(func() { close(started); <-release })
		}
	}
	reportA := recoverAsync(ctx, c.a)
	<-started
	require.Equal(t, 2, c.count(t, `SELECT count(*) FROM purchases WHERE lease_owner IS NOT NULL`), "worker A claimed both")
	c.exec(t, false, `UPDATE purchases SET lease_until = now() - interval '1 second' WHERE id = $1::uuid`, ids[1])

	reportB, err := c.b.orders.RecoverOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reportB.Confirmed, "worker B took the purchase whose lease expired")
	close(release)
	gotA := <-reportA
	require.NoError(t, gotA.err)
	a := gotA.report

	require.Equal(t, 1, a.Confirmed)
	require.Equal(t, 1, a.Skipped, "worker A's renewal found the purchase taken and did not charge it")
	require.Equal(t, 2, c.pay.chargesFor(ids[1]), "the checkout and worker B, never worker A")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM payment_fake_charges WHERE reference = $1`, ids[1]))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM inventory_movements WHERE reason = 'sale' AND reference = $1`, ids[1]), "finalized once")
	require.Equal(t, 1, c.a.logs.count("INFO", "another worker took or settled"))
	for _, id := range ids {
		status, _ := c.status(t, id)
		require.Equal(t, "CONFIRMED", status)
	}
}

func TestAStuckPurchaseIsLoggedOnceAcrossWorkersAndResolvedLater(t *testing.T) {
	c := newCluster(t, 3)
	ctx := context.Background()
	c.pay.set(false, false)
	p := c.buy(t, "alice", c.product(t, "A-1", 5), "pm_unknown")
	c.age(t, "1 minute")
	c.pay.set(true, false)

	for i := range 5 {
		worker := []replica{c.a, c.b}[i%2]
		_, err := worker.orders.RecoverOnce(ctx)
		require.NoError(t, err)
		c.due(t)
	}
	errorsLogged := c.a.logs.count("ERROR", "stuck in recovery") + c.b.logs.count("ERROR", "stuck in recovery")
	require.Equal(t, 1, errorsLogged, "the worker whose reschedule returned the threshold logs it, once")
	require.Equal(t, 2, c.a.logs.count("WARN", "still pending")+c.b.logs.count("WARN", "still pending"), "attempts 4 and 5")
	require.Equal(t, float64(1), c.a.metrics.gauge("checkout_recovery_stuck"), "worker A ran the last cycle")
	require.Equal(t, float64(2), c.a.metrics.gauge("checkout_recovery_stuck_reserved_units"))

	c.pay.set(false, false)
	_, err := c.a.orders.RecoverOnce(ctx)
	require.NoError(t, err)
	status, _ := c.status(t, p.ID)
	require.Equal(t, "CONFIRMED", status, "the lookup found the lost approval")
	require.Equal(t, 1, c.a.logs.count("INFO", "a stuck purchase was resolved"))
	require.Contains(t, c.a.logs.buf.String(), `"attempt":5`)
	require.Equal(t, float64(0), c.a.metrics.gauge("checkout_recovery_stuck"))
	require.Equal(t, int64(1), c.a.metrics.counter("checkout_recovery_resolutions_total|confirmed"))
}

func overdue(t *testing.T, c cluster, token string) (ordering.Purchase, catalog.Product) {
	t.Helper()
	product := c.product(t, "A-1", 5)
	p := c.buy(t, "alice", product, token)
	c.age(t, "31 minutes")
	return p, product
}

func reserved(t *testing.T, c cluster, product catalog.Product) int {
	t.Helper()
	return c.count(t, `SELECT reserved FROM products WHERE id = $1::uuid`, product.ID)
}

func TestAnOverduePurchaseThatNeverReachedTheProviderIsVoided(t *testing.T) {
	c := newCluster(t, 10)
	ctx := context.Background()
	c.pay.set(false, true)
	p, product := overdue(t, c, "pm_approved")
	c.pay.set(false, false)
	require.Equal(t, 2, reserved(t, c, product))

	report, err := c.a.orders.RecoverOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, report.Voided)
	status, reason := c.status(t, p.ID)
	require.Equal(t, []string{"FAILED", "payment_not_completed"}, []string{status, reason})
	require.Zero(t, reserved(t, c, product), "the stock is released")
	require.Equal(t, int64(1), c.a.metrics.counter("checkout_recovery_resolutions_total|voided"))

	late, err := c.pay.real.Charge(ctx, payment.ChargeRequest{Reference: p.ID, Token: "pm_approved", Amount: p.Total, Currency: p.Currency})
	require.NoError(t, err)
	require.Equal(t, payment.ChargeResult{Status: payment.StatusDeclined, Reason: payment.ReasonVoided}, late, "a late charge is rejected")
	require.Zero(t, c.count(t, `SELECT count(*) FROM payment_fake_charges WHERE reference = $1 AND status = 'APPROVED'`, p.ID))
}

func TestAChargeThatArrivesBeforeTheVoidConfirmsThePurchase(t *testing.T) {
	c := newCluster(t, 10)
	ctx := context.Background()
	c.pay.set(false, true)
	p, product := overdue(t, c, "pm_approved")
	c.pay.set(false, false)
	c.pay.beforeVoid = func(reference string) {
		_, err := c.pay.real.Charge(ctx, payment.ChargeRequest{Reference: reference, Token: "pm_approved", Amount: p.Total, Currency: p.Currency})
		require.NoError(t, err)
	}

	_, err := c.a.orders.RecoverOnce(ctx)
	require.NoError(t, err)
	status, _ := c.status(t, p.ID)
	require.Equal(t, "CONFIRMED", status)
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM payment_fake_charges WHERE reference = $1`, p.ID), "a single charge")
	require.Equal(t, 3, c.count(t, `SELECT stock FROM products WHERE id = $1::uuid`, product.ID))
	require.Equal(t, int64(1), c.a.metrics.counter("checkout_recovery_resolutions_total|confirmed"))
}

func TestALostApprovalPastTheDeadlineIsConfirmedByLookup(t *testing.T) {
	c := newCluster(t, 10)
	p, _ := overdue(t, c, "pm_unknown")
	_, err := c.a.orders.RecoverOnce(context.Background())
	require.NoError(t, err)
	status, _ := c.status(t, p.ID)
	require.Equal(t, "CONFIRMED", status)
	require.Equal(t, 1, c.pay.chargesFor(p.ID), "the recovery looks up instead of charging again")
}

func TestALookupWithAnotherAmountKeepsThePurchasePending(t *testing.T) {
	c := newCluster(t, 10)
	p, _ := overdue(t, c, "pm_unknown")
	c.exec(t, false, `UPDATE payment_fake_charges SET amount = 1.00 WHERE reference = $1`, p.ID)

	_, err := c.a.orders.RecoverOnce(context.Background())
	require.NoError(t, err)
	status, _ := c.status(t, p.ID)
	require.Equal(t, "PENDING", status, "a different charge is never confirmed")
	require.Equal(t, 1, c.a.logs.count("ERROR", "another amount or currency"))
	require.Equal(t, int64(1), c.a.metrics.counter("checkout_recovery_resolutions_total|mismatch"))
}

func TestAnUnansweringProviderLeavesTheOverduePurchaseOpen(t *testing.T) {
	c := newCluster(t, 10)
	p, product := overdue(t, c, "pm_unknown")
	c.pay.set(true, false)

	_, err := c.a.orders.RecoverOnce(context.Background())
	require.NoError(t, err)
	status, _ := c.status(t, p.ID)
	require.Equal(t, "PENDING", status)
	require.Equal(t, 2, reserved(t, c, product))
	require.Equal(t, int64(1), c.a.metrics.counter("checkout_recovery_resolutions_total|error"))
	require.Equal(t, float64(1), c.a.metrics.gauge("checkout_pending_purchases"))
	require.Zero(t, c.count(t, `SELECT count(*) FROM payment_fake_charges WHERE status = 'VOIDED'`))
}

func TestAYoungPurchaseBelowTheThresholdIsNeverVoided(t *testing.T) {
	c := newCluster(t, 10)
	c.pay.set(false, true)
	p := c.buy(t, "alice", c.product(t, "A-1", 5), "pm_approved")
	c.age(t, "1 minute")

	_, err := c.a.orders.RecoverOnce(context.Background())
	require.NoError(t, err)
	status, _ := c.status(t, p.ID)
	require.Equal(t, "PENDING", status)
	require.Zero(t, c.count(t, `SELECT count(*) FROM payment_fake_charges WHERE reference = $1`, p.ID), "neither charged nor voided")
	require.Zero(t, c.a.metrics.counter("checkout_recovery_resolutions_total|voided"))
}

func TestAPurchaseYoungerThanTheMinimumAgeIsNotTouched(t *testing.T) {
	c := newCluster(t, 10)
	c.pay.set(false, true)
	p := c.buy(t, "alice", c.product(t, "A-1", 5), "pm_approved")
	c.pay.set(false, false)
	charges := c.pay.chargesFor(p.ID)

	report, err := c.a.orders.RecoverOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, ordering.RecoveryReport{}, report, "a checkout may still be settling it")
	require.Equal(t, charges, c.pay.chargesFor(p.ID))
}

func TestTwoWorkersResolvingTheSamePurchaseApplyOneEffect(t *testing.T) {
	c := newCluster(t, 10)
	ctx := context.Background()
	c.pay.set(false, true)
	p, product := overdue(t, c, "pm_approved")
	c.pay.set(false, false)

	inLookup, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	c.pay.onLookup = func(string) {
		if blocked.CompareAndSwap(false, true) {
			close(inLookup)
			<-release
		}
	}
	done := recoverAsync(ctx, c.a)
	<-inLookup
	c.exec(t, false, `UPDATE purchases SET lease_until = now() - interval '1 second' WHERE id = $1::uuid`, p.ID)
	reportB, err := c.b.orders.RecoverOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reportB.Voided)
	close(release)
	gotA := <-done
	require.NoError(t, gotA.err)
	reportA := gotA.report

	require.Zero(t, reportA.Voided, "worker A's finalization found the purchase settled")
	require.Equal(t, 1, reportA.Skipped)
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM inventory_movements WHERE reason = 'reservation_release' AND reference = $1`, p.ID))
	require.Zero(t, reserved(t, c, product))
	require.Equal(t, int64(1), c.a.metrics.counter("checkout_recovery_resolutions_total|voided")+c.b.metrics.counter("checkout_recovery_resolutions_total|voided"))
}

func TestStoppingTheWorkerLetsThePurchaseInProgressFinish(t *testing.T) {
	c := newCluster(t, 10)
	c.pay.set(false, true)
	p := c.buy(t, "alice", c.product(t, "A-1", 5), "pm_approved")
	c.age(t, "1 minute")
	c.pay.set(false, false)

	charging, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.pay.onCharge = func(string) { once.Do(func() { close(charging); <-release }) }
	worker := c.a.orders.RecoveryWorker()
	worker.Start(context.Background())
	<-charging

	stopped := make(chan error, 1)
	go func() { stopped <- worker.Stop(context.Background()) }()
	select {
	case <-stopped:
		t.Fatal("the worker stopped in the middle of a purchase")
	default:
	}
	close(release)
	require.NoError(t, <-stopped)
	status, _ := c.status(t, p.ID)
	require.Equal(t, "CONFIRMED", status, "the purchase in progress finished within its budgets")
	require.Zero(t, c.count(t, `SELECT count(*) FROM purchases WHERE lease_owner IS NOT NULL AND status = 'PENDING'`))
}

func TestAPurchaseThatAlwaysFailsDoesNotBlockTheOthers(t *testing.T) {
	c := newCluster(t, 10)
	c.pay.set(false, true)
	stuck := c.buy(t, "alice", c.product(t, "A-1", 5), "pm_approved")
	others := []ordering.Purchase{c.buy(t, "bob", c.product(t, "B-1", 5), "pm_approved")}
	c.age(t, "1 minute")
	c.pay.set(false, false)
	c.pay.failing = stuck.ID

	report, err := c.a.orders.RecoverOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.Confirmed)
	require.Len(t, report.StillPending, 1)
	require.Equal(t, stuck.ID, report.StillPending[0].ID)
	status, _ := c.status(t, others[0].ID)
	require.Equal(t, "CONFIRMED", status)
	require.Equal(t, 1, c.count(t, `SELECT recovery_attempts FROM purchases WHERE id = $1::uuid`, stuck.ID))
}

func TestTwoReplicasCheckingOutAndRecoveringChargeAndSettleEachPurchaseOnce(t *testing.T) {
	c := newCluster(t, 10)
	ctx := context.Background()
	product := c.product(t, "A-1", 100)
	var wg sync.WaitGroup
	for i, r := range []replica{c.a, c.b, c.a, c.b, c.a, c.b} {
		user := []string{"alice", "bob"}[i%2]
		wg.Go(func() {
			key := uuid.NewString()
			if err := r.orders.AddToCart(ctx, user, product.ID, 1); err != nil {
				return
			}
			_, _ = r.orders.CheckoutCart(ctx, user, key, "pm_unknown")
		})
	}
	wg.Wait()
	pending := c.count(t, `SELECT count(*) FROM purchases WHERE status = 'PENDING'`)
	require.Positive(t, pending)
	c.age(t, "1 minute")

	results := []<-chan recovered{recoverAsync(ctx, c.a), recoverAsync(ctx, c.b)}
	for _, result := range results {
		require.NoError(t, (<-result).err)
	}

	require.Zero(t, c.count(t, `SELECT count(*) FROM purchases WHERE status = 'PENDING'`))
	require.Zero(t, c.count(t, `
		SELECT count(*) FROM purchases p
		WHERE (SELECT count(*) FROM payment_fake_charges f WHERE f.reference = p.id::text) <> 1
		   OR (SELECT count(*) FROM inventory_movements m WHERE m.reason = 'sale' AND m.reference = p.id::text) <> 1`),
		"every purchase was charged once in the fake and finalized once")
	require.Zero(t, c.count(t, `SELECT reserved FROM products WHERE id = $1::uuid`, product.ID))
}
