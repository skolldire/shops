//go:build integration

package ordering

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/postgres"
	"github.com/skolldire/shops/internal/payment"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

type markBarrier struct {
	app.EventQueue
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	opened  sync.Once
}

func newMarkBarrier(queue app.EventQueue) *markBarrier {
	return &markBarrier{EventQueue: queue, entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *markBarrier) open() {
	b.opened.Do(func() { close(b.release) })
}

func (b *markBarrier) MarkPublished(ctx context.Context, seq int64, owner string) (bool, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.EventQueue.MarkPublished(ctx, seq, owner)
}

type rescheduleBarrier struct {
	app.EventQueue
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	opened  sync.Once
}

func (b *rescheduleBarrier) open() {
	b.opened.Do(func() { close(b.release) })
}

func (b *rescheduleBarrier) Reschedule(ctx context.Context, seq int64, owner string, backoff time.Duration, cause string) (int, bool, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.EventQueue.Reschedule(ctx, seq, owner, backoff, cause)
}

type relayFixture struct {
	pool *pgxpool.Pool
	cat  *catalog.Module
	pay  *payment.Module
}

func newRelayFixture(t *testing.T) relayFixture {
	t.Helper()
	pool, _ := dbtest.Start(t)
	quiet, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	cat, err := catalog.NewModule(catalog.Deps{Pool: pool, Log: quiet, Metrics: telemetry.Noop(), Tracer: telemetry.Noop(), Import: catalog.ImportLimits{MaxBytes: 1 << 20, MaxRows: 100}})
	require.NoError(t, err)
	pay, err := payment.NewModule(pool)
	require.NoError(t, err)
	return relayFixture{pool: pool, cat: cat, pay: pay}
}

func (f relayFixture) module(t *testing.T, wrap func(app.EventQueue) app.EventQueue) *Module {
	t.Helper()
	manager, err := database.NewTxManager(f.pool)
	require.NoError(t, err)
	tx, err := postgres.NewTx(manager)
	require.NoError(t, err)
	reservation, err := postgres.NewReservationTx(manager, f.pool, time.Second)
	require.NoError(t, err)
	purchases, err := postgres.NewPurchases(f.pool)
	require.NoError(t, err)
	carts, err := postgres.NewCarts(f.pool, manager)
	require.NoError(t, err)
	outbox, err := postgres.NewOutbox(f.pool)
	require.NoError(t, err)
	inbox, err := postgres.NewInbox(f.pool)
	require.NoError(t, err)
	sales, err := postgres.NewSales(f.pool)
	require.NoError(t, err)
	quiet, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	m, err := newModule(ports{
		tx: tx, reservation: reservation, created: purchases, reader: purchases, finisher: purchases, pending: purchases,
		editor: carts, remover: carts, events: outbox, queue: wrap(outbox), inbox: inbox, sales: sales,
	}, Deps{
		Catalog: f.cat, Payments: f.pay, Log: quiet, Metrics: telemetry.Noop(), Users: []string{"alice", "bob"},
		Budgets: Budgets{Reserve: 3 * time.Second, LockTimeout: time.Second, Charge: 3 * time.Second, Finalize: 3 * time.Second},
		Recovery: RecoveryConfig{
			Interval: time.Hour, BatchSize: 20, MinAge: 30 * time.Second, Lease: 30 * time.Second, DBTimeout: 2 * time.Second,
			BaseBackoff: time.Second, MaxBackoff: time.Minute, StuckAfterAttempts: 10, MaxPendingAge: 30 * time.Minute,
		},
		Events: EventsConfig{
			Interval: time.Hour, BatchSize: 10, Lease: 10 * time.Second, DBTimeout: 2 * time.Second, HandlerTimeout: 2 * time.Second,
			BaseBackoff: time.Second, MaxBackoff: time.Minute, StuckAfterAttempts: 10,
		},
	})
	require.NoError(t, err)
	return m
}

func (f relayFixture) confirmed(t *testing.T, m *Module, user string, units int64) string {
	t.Helper()
	ctx := context.Background()
	price, stock, weight := decimal.RequireFromString("10.00"), int64(100), decimal.RequireFromString("1")
	p, err := f.cat.Create(ctx, catalog.CreateInput{SKU: "SKU-" + uuid.NewString()[:8], Name: "Item", Category: "Tests", Price: &price, Stock: &stock, WeightKg: &weight})
	require.NoError(t, err)
	require.NoError(t, m.AddToCart(ctx, user, p.ID, int(units)))
	res, err := m.CheckoutCart(ctx, user, uuid.NewString(), "pm_approved")
	require.NoError(t, err)
	require.Equal(t, StatusConfirmed, res.Purchase.Status)
	return p.ID
}

func (f relayFixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

type relayed struct {
	report RelayReport
	err    error
}

func TestARelayWhoseLeaseExpiredBeforeConfirmingChangesNothing(t *testing.T) {
	f := newRelayFixture(t)
	ctx := context.Background()
	barrier := newMarkBarrier(nil)
	a := f.module(t, func(q app.EventQueue) app.EventQueue { barrier.EventQueue = q; return barrier })
	b := f.module(t, func(q app.EventQueue) app.EventQueue { return q })
	t.Cleanup(barrier.open)
	product := f.confirmed(t, a, "alice", 3)

	first := make(chan relayed, 1)
	go func() {
		report, err := a.RelayOnce(ctx)
		first <- relayed{report, err}
	}()
	<-barrier.entered
	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM inbox`), "worker A consumed the event and stopped before confirming it")
	_, err := f.pool.Exec(ctx, `UPDATE outbox SET lease_until = now() - interval '1 second'`)
	require.NoError(t, err)

	reportB, err := b.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, RelayReport{Published: 1, Duplicates: 1}, reportB, "worker B claimed the expired lease, the inbox absorbed its delivery and it confirmed")

	barrier.open()
	gotA := <-first
	require.NoError(t, gotA.err)
	require.Equal(t, RelayReport{Skipped: 1}, gotA.report, "the late confirmation of A changes no row")

	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM inbox`))
	require.Equal(t, 3, f.count(t, `SELECT units_sold FROM product_sales WHERE product_id = $1::uuid`, product), "one effect")
	require.Equal(t, 1, f.count(t, `SELECT purchases FROM product_sales WHERE product_id = $1::uuid`, product))
	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM outbox WHERE published_at IS NOT NULL AND lease_owner IS NULL`), "one published_at")
}

func TestStoppingTheRelayFinishesTheEventInProgressAndStartsNoOther(t *testing.T) {
	f := newRelayFixture(t)
	barrier := newMarkBarrier(nil)
	m := f.module(t, func(q app.EventQueue) app.EventQueue { barrier.EventQueue = q; return barrier })
	t.Cleanup(barrier.open)
	firstProduct := f.confirmed(t, m, "alice", 1)
	secondProduct := f.confirmed(t, m, "bob", 2)

	ctx, stop := context.WithCancel(context.Background())
	worker := m.RelayWorker()
	worker.Start(ctx)
	<-barrier.entered
	stop()
	barrier.open()
	deadline, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, worker.Stop(deadline))

	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM outbox WHERE published_at IS NOT NULL`), "the event in progress was confirmed")
	require.Equal(t, 1, f.count(t, `SELECT units_sold FROM product_sales WHERE product_id = $1::uuid`, firstProduct))
	require.Zero(t, f.count(t, `SELECT count(*) FROM product_sales WHERE product_id = $1::uuid`, secondProduct), "the next event of the batch was not started")
	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM outbox WHERE published_at IS NULL`))
}

func TestARelayThatLostTheLeaseOfAnUnreadableEventDoesNotRescheduleIt(t *testing.T) {
	f := newRelayFixture(t)
	ctx := context.Background()
	barrier := &rescheduleBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	a := f.module(t, func(q app.EventQueue) app.EventQueue { barrier.EventQueue = q; return barrier })
	b := f.module(t, func(q app.EventQueue) app.EventQueue { return q })
	t.Cleanup(barrier.open)
	f.confirmed(t, a, "alice", 2)
	_, err := f.pool.Exec(ctx, `UPDATE outbox SET payload = jsonb_set(payload, '{total}', '"not-a-decimal"')`)
	require.NoError(t, err)

	first := make(chan relayed, 1)
	go func() {
		report, err := a.RelayOnce(ctx)
		first <- relayed{report, err}
	}()
	select {
	case <-barrier.entered:
	case got := <-first:
		t.Fatalf("worker A never rescheduled the unreadable event: %+v", got)
	}
	_, err = f.pool.Exec(ctx, `UPDATE outbox SET lease_until = now() - interval '1 second'`)
	require.NoError(t, err)

	reportB, err := b.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, RelayReport{StillPending: 1}, reportB, "worker B took the expired lease and rescheduled the event")

	barrier.open()
	gotA := <-first
	require.NoError(t, gotA.err)
	require.Equal(t, RelayReport{StillPending: 1}, gotA.report, "A counts its own attempt as pending")
	require.Equal(t, 1, f.count(t, `SELECT attempts FROM outbox`), "the reschedule of A changed no row")
	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM outbox WHERE published_at IS NULL AND lease_owner IS NULL AND next_attempt_at > now()`))
	require.Zero(t, f.count(t, `SELECT count(*) FROM inbox`))
}
