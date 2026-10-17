//go:build integration

package ordering_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
)

const stuckEventsAfter = 3

func eventCluster(t *testing.T) cluster {
	t.Helper()
	events := eventsConfig
	events.DBTimeout, events.HandlerTimeout, events.StuckAfterAttempts = 500*time.Millisecond, 500*time.Millisecond, stuckEventsAfter
	return buildCluster(t, recoveryConfig, events)
}

func (c cluster) checkout(t *testing.T, r replica, user, token string, lines map[catalog.Product]int) ordering.Purchase {
	t.Helper()
	ctx := context.Background()
	for p, qty := range lines {
		require.NoError(t, r.orders.AddToCart(ctx, user, p.ID, qty))
	}
	res, err := r.orders.CheckoutCart(ctx, user, uuid.NewString(), token)
	require.NoError(t, err)
	return res.Purchase
}

func (c cluster) unpublished(t *testing.T) int {
	t.Helper()
	return c.count(t, `SELECT count(*) FROM outbox WHERE published_at IS NULL`)
}

func (c cluster) drain(t *testing.T, r replica) {
	t.Helper()
	for range 20 {
		if c.unpublished(t) == 0 {
			return
		}
		_, err := r.orders.RelayOnce(context.Background())
		require.NoError(t, err)
	}
	require.Zero(t, c.unpublished(t), "the relay published every event")
}

func (c cluster) dueEvents(t *testing.T) {
	t.Helper()
	c.exec(t, false, `UPDATE outbox SET next_attempt_at = now() WHERE published_at IS NULL`)
}

func TestEveryFinishedPurchaseRecordsOneEventWithoutSensitiveData(t *testing.T) {
	c := eventCluster(t)
	c.pay.set(false, false)
	tent := c.product(t, "TENT-1", 10)
	confirmed := c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 2})
	declined := c.checkout(t, c.a, "bob", "pm_declined", map[catalog.Product]int{tent: 1})
	require.Equal(t, ordering.StatusConfirmed, confirmed.Status)
	require.Equal(t, ordering.StatusFailed, declined.Status)
	require.Equal(t, 2, c.count(t, `SELECT count(*) FROM outbox`))

	for _, tc := range []struct {
		purchase ordering.Purchase
		kind     string
	}{{confirmed, "PurchaseConfirmed"}, {declined, "PurchaseFailed"}} {
		var aggregateType, kind, correlation, payload, eventID string
		var version int
		require.NoError(t, c.pool.QueryRow(context.Background(),
			`SELECT aggregate_type, type, version, correlation_id, payload::text, event_id::text FROM outbox WHERE aggregate_id = $1::uuid`, tc.purchase.ID).
			Scan(&aggregateType, &kind, &version, &correlation, &payload, &eventID))
		require.Equal(t, "Purchase", aggregateType)
		require.Equal(t, tc.kind, kind)
		require.Equal(t, 1, version)
		require.Equal(t, "purchase:"+tc.purchase.ID, correlation)
		require.NotEmpty(t, eventID)
		for _, secret := range []string{"pm_", "alice", "bob", "token", "user"} {
			require.NotContains(t, payload, secret)
		}
	}

	var total, unitPrice, totalType, priceType, reason string
	require.NoError(t, c.pool.QueryRow(context.Background(),
		`SELECT payload->>'total', payload->'items'->0->>'unit_price', jsonb_typeof(payload->'total'), jsonb_typeof(payload->'items'->0->'unit_price')
		 FROM outbox WHERE aggregate_id = $1::uuid`, confirmed.ID).Scan(&total, &unitPrice, &totalType, &priceType))
	require.Equal(t, []string{"20.00", "10.00", "string", "string"}, []string{total, unitPrice, totalType, priceType}, "amounts are exact decimal text")
	require.NoError(t, c.pool.QueryRow(context.Background(), `SELECT payload->>'reason' FROM outbox WHERE aggregate_id = $1::uuid`, declined.ID).Scan(&reason))
	require.Equal(t, "card_declined", reason)
}

func TestARepeatedFinalizationRecordsNoSecondEvent(t *testing.T) {
	c := eventCluster(t)
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

	require.Equal(t, 1, gotA.report.Skipped, "the second finalization loses the update")
	require.Empty(t, gotA.report.StillPending, "and fails on nothing, because it never tries to append")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM outbox WHERE aggregate_id = $1::uuid`, p.ID))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM inventory_movements WHERE reason = 'reservation_release' AND reference = $1`, p.ID), "the stock was settled once")
	require.Zero(t, reserved(t, c, product))
	require.Zero(t, c.a.logs.count("ERROR", "append the purchase event")+c.a.logs.count("INFO", "append the purchase event"))
}

func TestAnEventThatCannotBeAppendedRollsTheWholeFinalizationBack(t *testing.T) {
	c := eventCluster(t)
	ctx := context.Background()
	c.pay.set(false, true)
	product := c.product(t, "A-1", 5)
	p := c.buy(t, "alice", product, "pm_approved")
	c.age(t, "1 minute")
	c.pay.set(false, false)
	var synthetic string
	require.NoError(t, c.pool.QueryRow(ctx,
		`INSERT INTO outbox (aggregate_type, aggregate_id, type, version, correlation_id, payload)
		 VALUES ('Purchase', $1::uuid, 'PurchaseFailed', 1, 'synthetic', '{"synthetic": true}') RETURNING event_id::text`, p.ID).Scan(&synthetic))
	movements := c.count(t, `SELECT count(*) FROM inventory_movements`)
	cart := c.count(t, `SELECT count(*) FROM cart_items WHERE user_id = 'alice'`)

	report, err := c.a.orders.RecoverOnce(ctx)
	require.NoError(t, err)
	require.Len(t, report.StillPending, 1, "the finalization failed on the unique index")
	require.ErrorContains(t, report.StillPending[0].Cause, "outbox_one_event_per_purchase")
	status, _ := c.status(t, p.ID)
	require.Equal(t, "PENDING", status, "the state change was rolled back")
	require.Equal(t, 2, reserved(t, c, product), "the sale was rolled back")
	require.Equal(t, 5, c.count(t, `SELECT stock FROM products WHERE id = $1::uuid`, product.ID))
	require.Equal(t, movements, c.count(t, `SELECT count(*) FROM inventory_movements`))
	require.Equal(t, cart, c.count(t, `SELECT count(*) FROM cart_items WHERE user_id = 'alice'`), "the cart cleanup was rolled back")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM outbox WHERE aggregate_id = $1::uuid`, p.ID))
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM outbox WHERE event_id = $1::uuid AND payload = '{"synthetic": true}'`, synthetic), "the synthetic event is intact")
}

func TestARedeliveredEventDoesNotChangeTheProjectionTwice(t *testing.T) {
	c := eventCluster(t)
	ctx := context.Background()
	c.pay.set(false, false)
	tent := c.product(t, "TENT-1", 10)
	p := c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 3})

	first, err := c.a.orders.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RelayReport{Published: 1}, first)
	c.exec(t, false, `UPDATE outbox SET published_at = NULL WHERE aggregate_id = $1::uuid`, p.ID)

	again, err := c.a.orders.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RelayReport{Published: 1, Duplicates: 1}, again, "the same event_id is delivered again")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM inbox`))
	require.Equal(t, 3, c.count(t, `SELECT units_sold FROM product_sales WHERE product_id = $1::uuid`, tent.ID))
	require.Equal(t, 1, c.count(t, `SELECT purchases FROM product_sales WHERE product_id = $1::uuid`, tent.ID))
	require.Equal(t, int64(1), c.a.metrics.counter("inbox_duplicates_total"))
}

func TestTwoRelaysPublishEachEventOnce(t *testing.T) {
	c := eventCluster(t)
	ctx := context.Background()
	c.pay.set(false, false)
	products := []catalog.Product{c.product(t, "A-1", 50), c.product(t, "B-1", 50)}
	for i := range 6 {
		user := []string{"alice", "bob"}[i%2]
		c.checkout(t, c.a, user, "pm_approved", map[catalog.Product]int{products[i%2]: i + 1})
	}

	first, second := relayAsync(ctx, c.a), relayAsync(ctx, c.b)
	a, b := <-first, <-second
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	c.drain(t, c.a)

	require.Equal(t, 6, a.report.Published+b.report.Published, "every event was published by exactly one relay")
	require.Zero(t, a.report.Duplicates+b.report.Duplicates)
	require.Equal(t, 6, c.count(t, `SELECT count(*) FROM inbox`))
	requireProjectionMatchesTheSource(t, c)
}

type relayed struct {
	report ordering.RelayReport
	err    error
}

func relayAsync(ctx context.Context, r replica) <-chan relayed {
	out := make(chan relayed, 1)
	go func() {
		report, err := r.orders.RelayOnce(ctx)
		out <- relayed{report: report, err: err}
	}()
	return out
}

func (c cluster) blockProjection(t *testing.T, product catalog.Product) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := c.pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT 1 FROM product_sales WHERE product_id = $1::uuid FOR UPDATE`, product.ID)
	require.NoError(t, err)
	released := false
	release := func() {
		if !released {
			released = true
			require.NoError(t, tx.Rollback(ctx))
		}
	}
	t.Cleanup(release)
	return release
}

func TestAFailingConsumerIsRetriedWithBackoffAndThenRecovers(t *testing.T) {
	c := eventCluster(t)
	ctx := context.Background()
	c.pay.set(false, false)
	tent := c.product(t, "TENT-1", 10)
	c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 1})
	c.drain(t, c.a)
	p := c.checkout(t, c.a, "bob", "pm_approved", map[catalog.Product]int{tent: 2})
	release := c.blockProjection(t, tent)

	report, err := c.a.orders.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RelayReport{StillPending: 1}, report, "the projection did not answer within the handler timeout")
	var attempts int
	var future bool
	var lastError string
	require.NoError(t, c.pool.QueryRow(ctx, `SELECT attempts, next_attempt_at > now(), last_error, lease_owner IS NULL FROM outbox WHERE aggregate_id = $1::uuid`, p.ID).
		Scan(&attempts, &future, &lastError, new(bool)))
	require.Equal(t, 1, attempts)
	require.True(t, future, "the next attempt waits for the backoff")
	require.NotEmpty(t, lastError)
	again, err := c.a.orders.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RelayReport{}, again, "nothing is due before the backoff ends")

	for range stuckEventsAfter - 1 {
		c.dueEvents(t)
		_, err := c.a.orders.RelayOnce(ctx)
		require.NoError(t, err)
	}
	require.Equal(t, 1, c.a.logs.count("ERROR", "an outbox event is stuck"))
	require.Equal(t, float64(1), c.a.metrics.gauge("outbox_stuck_events"))

	release()
	c.dueEvents(t)
	recovered, err := c.a.orders.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RelayReport{Published: 1}, recovered)
	require.Equal(t, 3, c.count(t, `SELECT units_sold FROM product_sales WHERE product_id = $1::uuid`, tent.ID))
	require.Equal(t, float64(0), c.a.metrics.gauge("outbox_stuck_events"))
}

func TestAStuckEventIsLoggedOnceAcrossRelays(t *testing.T) {
	c := eventCluster(t)
	ctx := context.Background()
	c.pay.set(false, false)
	tent := c.product(t, "TENT-1", 10)
	c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 1})
	c.drain(t, c.a)
	c.checkout(t, c.a, "bob", "pm_approved", map[catalog.Product]int{tent: 2})
	c.blockProjection(t, tent)

	for i := range 2 * stuckEventsAfter {
		relay := []replica{c.a, c.b}[i%2]
		report, err := relay.orders.RelayOnce(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, report.StillPending)
		c.dueEvents(t)
	}
	require.Equal(t, 1, c.a.logs.count("ERROR", "an outbox event is stuck")+c.b.logs.count("ERROR", "an outbox event is stuck"),
		"only the relay whose reschedule returned the threshold logs it")
	require.Equal(t, 1, c.a.logs.count("WARN", "a stuck outbox event is still pending")+c.b.logs.count("WARN", "a stuck outbox event is still pending"),
		"one warning at twice the threshold")
	require.Equal(t, 2*stuckEventsAfter, c.count(t, `SELECT max(attempts) FROM outbox`))
}

func requireProjectionMatchesTheSource(t *testing.T, c cluster) {
	t.Helper()
	require.Zero(t, c.count(t, `
		SELECT count(*) FROM (
		    SELECT i.product_id, sum(i.quantity) AS units, count(*) AS purchases
		    FROM purchase_items i JOIN purchases p ON p.id = i.purchase_id
		    WHERE p.status = 'CONFIRMED'
		    GROUP BY i.product_id
		) source
		FULL JOIN product_sales s ON s.product_id = source.product_id
		WHERE source.product_id IS NULL OR s.product_id IS NULL
		   OR s.units_sold <> source.units OR s.purchases <> source.purchases`), "the projection equals the confirmed purchase items")
}

func TestTheProjectionMatchesTheConfirmedPurchasesOnly(t *testing.T) {
	c := eventCluster(t)
	c.pay.set(false, false)
	tent, lamp := c.product(t, "TENT-1", 50), c.product(t, "LAMP-1", 50)
	c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 2})
	c.checkout(t, c.a, "bob", "pm_approved", map[catalog.Product]int{tent: 1, lamp: 3})
	c.checkout(t, c.a, "alice", "pm_declined", map[catalog.Product]int{lamp: 5})
	c.checkout(t, c.b, "bob", "pm_unknown", map[catalog.Product]int{tent: 4})
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM purchases WHERE status = 'PENDING'`))
	require.Equal(t, 3, c.count(t, `SELECT count(*) FROM outbox`), "a pending purchase has no event yet")

	c.drain(t, c.b)
	requireProjectionMatchesTheSource(t, c)
	require.Equal(t, 3, c.count(t, `SELECT units_sold FROM product_sales WHERE product_id = $1::uuid`, tent.ID), "the pending 4 tents are not counted")
	require.Equal(t, 2, c.count(t, `SELECT purchases FROM product_sales WHERE product_id = $1::uuid`, tent.ID))
	require.Equal(t, 3, c.count(t, `SELECT units_sold FROM product_sales WHERE product_id = $1::uuid`, lamp.ID), "the declined 5 lamps are not counted")
	require.Equal(t, 3, c.count(t, `SELECT count(*) FROM inbox`), "the failed purchase is in the inbox too")
}
