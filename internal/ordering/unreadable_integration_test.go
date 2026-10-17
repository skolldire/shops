//go:build integration

package ordering_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
)

const (
	syntheticSecret = "SYNTHETIC-SECRET-7731"
	unreadableCause = "ordering: the event payload does not match its type and version: an amount that is not a decimal"
)

func (b *logBuffer) contains(text string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(b.buf.String(), text)
}

type pendingEvent struct {
	attempts  int
	lastError string
	leased    bool
	published bool
	next      time.Time
}

func (c cluster) event(t *testing.T, purchaseID string) pendingEvent {
	t.Helper()
	var e pendingEvent
	var lastError *string
	require.NoError(t, c.pool.QueryRow(context.Background(),
		`SELECT attempts, last_error, lease_owner IS NOT NULL OR lease_until IS NOT NULL, published_at IS NOT NULL, next_attempt_at
		 FROM outbox WHERE aggregate_id = $1::uuid`, purchaseID).Scan(&e.attempts, &lastError, &e.leased, &e.published, &e.next))
	if lastError != nil {
		e.lastError = *lastError
	}
	return e
}

func (c cluster) corruptTotal(t *testing.T, purchaseID string) string {
	t.Helper()
	var original string
	require.NoError(t, c.pool.QueryRow(context.Background(), `SELECT payload::text FROM outbox WHERE aggregate_id = $1::uuid`, purchaseID).Scan(&original))
	c.exec(t, false, `UPDATE outbox SET payload = jsonb_set(payload, '{total}', to_jsonb($2::text)) WHERE aggregate_id = $1::uuid`, purchaseID, syntheticSecret)
	return original
}

func (c cluster) dbNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, c.pool.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&now))
	return now
}

func (c cluster) noEffectOf(t *testing.T, purchaseID string) {
	t.Helper()
	require.Zero(t, c.count(t, `SELECT count(*) FROM inbox WHERE event_id = (SELECT event_id FROM outbox WHERE aggregate_id = $1::uuid)`, purchaseID),
		"the unreadable event is not recorded as processed")
}

func TestAnUnreadableEventDoesNotStopTheRestOfItsBatch(t *testing.T) {
	c := eventCluster(t)
	ctx := context.Background()
	c.pay.set(false, false)
	tent := c.product(t, "TENT-1", 50)
	poisoned := c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 1})
	c.checkout(t, c.a, "bob", "pm_approved", map[catalog.Product]int{tent: 2})
	c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 3})
	c.corruptTotal(t, poisoned.ID)

	before := c.dbNow(t)
	report, err := c.a.orders.RelayOnce(ctx)
	require.NoError(t, err)
	after := c.dbNow(t)
	require.Equal(t, ordering.RelayReport{Published: 2, StillPending: 1}, report, "the valid events of the batch are published in the same cycle")
	require.Equal(t, 5, c.count(t, `SELECT units_sold FROM product_sales WHERE product_id = $1::uuid`, tent.ID), "only the valid events are projected")
	require.Equal(t, 2, c.count(t, `SELECT purchases FROM product_sales WHERE product_id = $1::uuid`, tent.ID))
	require.Equal(t, 2, c.count(t, `SELECT count(*) FROM inbox`))
	c.noEffectOf(t, poisoned.ID)

	e := c.event(t, poisoned.ID)
	require.False(t, e.published, "an unreadable event is never published")
	require.Equal(t, 1, e.attempts)
	require.Equal(t, unreadableCause, e.lastError, "the cause names the category, not the content")
	require.False(t, e.leased, "the reschedule releases the lease")
	require.False(t, e.next.Before(before.Add(eventsConfig.BaseBackoff/2)), "the next attempt waits for the backoff")
	require.False(t, e.next.After(after.Add(eventsConfig.BaseBackoff)))
	require.Equal(t, float64(1), c.a.metrics.gauge("outbox_pending_events"))
}

func TestAnUnreadableEventReachesTheStuckThresholdWithoutExposingItsPayload(t *testing.T) {
	c := eventCluster(t)
	ctx := context.Background()
	c.pay.set(false, false)
	tent := c.product(t, "TENT-1", 50)
	poisoned := c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 1})
	c.corruptTotal(t, poisoned.ID)

	for attempt := 1; attempt <= stuckEventsAfter+2; attempt++ {
		report, err := c.a.orders.RelayOnce(ctx)
		require.NoError(t, err)
		require.Equal(t, ordering.RelayReport{StillPending: 1}, report)
		require.Equal(t, attempt, c.event(t, poisoned.ID).attempts, "every cycle counts one attempt")
		want := float64(0)
		if attempt >= stuckEventsAfter {
			want = 1
		}
		require.Equal(t, want, c.a.metrics.gauge("outbox_stuck_events"), "attempt %d", attempt)
		c.dueEvents(t)
	}
	require.Equal(t, 1, c.a.logs.count("ERROR", "an outbox event is stuck"), "one error when the threshold is crossed")
	require.Equal(t, 2, c.a.logs.count("WARN", "a stuck outbox event is still pending"), "warnings afterwards")
	require.Equal(t, float64(1), c.a.metrics.gauge("outbox_pending_events"))
	require.True(t, c.a.logs.contains("an amount that is not a decimal"), "the logs name the category")
	require.False(t, c.a.logs.contains(syntheticSecret), "the logs never carry the payload")
	require.NotContains(t, c.event(t, poisoned.ID).lastError, syntheticSecret)
	c.noEffectOf(t, poisoned.ID)
}

func TestAnEventOfAnUnsupportedVersionIsLeftPendingAndUntouched(t *testing.T) {
	c := eventCluster(t)
	ctx := context.Background()
	c.pay.set(false, false)
	tent := c.product(t, "TENT-1", 50)
	newer := c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 4})
	c.checkout(t, c.a, "bob", "pm_approved", map[catalog.Product]int{tent: 2})
	c.exec(t, false, `UPDATE outbox SET version = version + 1 WHERE aggregate_id = $1::uuid`, newer.ID)
	before := c.event(t, newer.ID)

	for range 3 {
		report, err := c.a.orders.RelayOnce(ctx)
		require.NoError(t, err)
		require.Zero(t, report.StillPending+report.Skipped, "the unsupported event is never claimed")
	}
	e := c.event(t, newer.ID)
	require.False(t, e.published, "it is never dropped or published")
	require.False(t, e.leased)
	require.Zero(t, e.attempts, "no attempt is counted for an event this replica cannot read")
	require.Empty(t, e.lastError)
	require.True(t, e.next.Equal(before.next), "no backoff is applied")
	require.Zero(t, c.a.logs.count("WARN", "")+c.a.logs.count("ERROR", ""), "no delivery log")
	require.Equal(t, float64(1), c.a.metrics.gauge("outbox_unsupported_events"))
	require.Equal(t, float64(1), c.a.metrics.gauge("outbox_pending_events"))
	c.noEffectOf(t, newer.ID)
	require.Equal(t, 2, c.count(t, `SELECT units_sold FROM product_sales WHERE product_id = $1::uuid`, tent.ID))
	require.Equal(t, 1, c.count(t, `SELECT purchases FROM product_sales WHERE product_id = $1::uuid`, tent.ID))

	c.exec(t, false, `UPDATE outbox SET version = version - 1 WHERE aggregate_id = $1::uuid`, newer.ID)
	repaired, err := c.a.orders.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RelayReport{Published: 1}, repaired, "a compatible version is consumed")
	require.Equal(t, float64(0), c.a.metrics.gauge("outbox_unsupported_events"))
	requireProjectionMatchesTheSource(t, c)
}

func TestARepairedEventIsPublishedOnceAndLeavesTheStuckEvents(t *testing.T) {
	c := eventCluster(t)
	ctx := context.Background()
	c.pay.set(false, false)
	tent := c.product(t, "TENT-1", 50)
	poisoned := c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 3})
	original := c.corruptTotal(t, poisoned.ID)
	var eventID string
	require.NoError(t, c.pool.QueryRow(ctx, `SELECT event_id::text FROM outbox WHERE aggregate_id = $1::uuid`, poisoned.ID).Scan(&eventID))

	for range stuckEventsAfter {
		_, err := c.a.orders.RelayOnce(ctx)
		require.NoError(t, err)
		c.dueEvents(t)
	}
	require.Equal(t, float64(1), c.a.metrics.gauge("outbox_stuck_events"))

	c.exec(t, false, `UPDATE outbox SET payload = $2::jsonb, next_attempt_at = now() WHERE aggregate_id = $1::uuid`, poisoned.ID, original)
	report, err := c.a.orders.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RelayReport{Published: 1}, report)
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM outbox WHERE event_id = $1::uuid AND published_at IS NOT NULL`, eventID), "the repair kept the event identity")
	require.Equal(t, float64(0), c.a.metrics.gauge("outbox_stuck_events"))
	require.Equal(t, float64(0), c.a.metrics.gauge("outbox_pending_events"))

	c.exec(t, false, `UPDATE outbox SET published_at = NULL WHERE event_id = $1::uuid`, eventID)
	again, err := c.a.orders.RelayOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RelayReport{Published: 1, Duplicates: 1}, again, "a redelivery is absorbed by the inbox")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM inbox WHERE consumer = 'sales_projection' AND event_id = $1::uuid`, eventID))
	require.Equal(t, 3, c.count(t, `SELECT units_sold FROM product_sales WHERE product_id = $1::uuid`, tent.ID))
	require.Equal(t, 1, c.count(t, `SELECT purchases FROM product_sales WHERE product_id = $1::uuid`, tent.ID))
	requireProjectionMatchesTheSource(t, c)
}

func TestAnInvalidProductIDIsSetAsideWithoutReachingTheDatabase(t *testing.T) {
	c := eventCluster(t)
	ctx := context.Background()
	c.pay.set(false, false)
	tent, lamp := c.product(t, "TENT-1", 50), c.product(t, "LAMP-1", 50)
	invalid := c.checkout(t, c.a, "alice", "pm_approved", map[catalog.Product]int{tent: 4})
	c.checkout(t, c.a, "bob", "pm_approved", map[catalog.Product]int{lamp: 2})
	marker := syntheticSecret + "-not-a-uuid"
	c.exec(t, false, `UPDATE outbox SET payload = jsonb_set(payload, '{items,0,product_id}', to_jsonb($2::text)) WHERE aggregate_id = $1::uuid`, invalid.ID, marker)

	before := c.dbNow(t)
	report, err := c.a.orders.RelayOnce(ctx)
	require.NoError(t, err)
	after := c.dbNow(t)
	require.Equal(t, ordering.RelayReport{Published: 1, StillPending: 1}, report, "the valid event of the batch is published")

	e := c.event(t, invalid.ID)
	require.False(t, e.published)
	require.Equal(t, 1, e.attempts)
	require.False(t, e.leased, "the reschedule releases the lease")
	require.False(t, e.next.Before(before.Add(eventsConfig.BaseBackoff/2)), "the next attempt waits for the backoff")
	require.False(t, e.next.After(after.Add(eventsConfig.BaseBackoff)))
	require.Equal(t, "ordering: the event payload does not match its type and version: missing or inconsistent fields", e.lastError)
	require.NotContains(t, e.lastError, syntheticSecret)
	require.False(t, c.a.logs.contains(syntheticSecret), "the logs never carry the payload")
	c.noEffectOf(t, invalid.ID)

	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM inbox`), "only the valid event is recorded as processed")
	require.Equal(t, 1, c.count(t, `SELECT count(*) FROM product_sales`), "only the valid event adds a product to the projection")
	require.Zero(t, c.count(t, `SELECT count(*) FROM product_sales WHERE product_id = $1::uuid`, tent.ID))
	require.Equal(t, 2, c.count(t, `SELECT units_sold FROM product_sales WHERE product_id = $1::uuid`, lamp.ID))
	require.Equal(t, 1, c.count(t, `SELECT purchases FROM product_sales WHERE product_id = $1::uuid`, lamp.ID))
}
