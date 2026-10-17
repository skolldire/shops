//go:build integration

package ordering_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
	"github.com/skolldire/shops/internal/payment"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

type barrierCatalog struct {
	*catalog.Module
	once    sync.Once
	opened  sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *barrierCatalog) open() {
	b.opened.Do(func() { close(b.release) })
}

func (b *barrierCatalog) ReserveStock(ctx context.Context, items []catalog.StockRequest, reference string) ([]catalog.StockLine, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.Module.ReserveStock(ctx, items, reference)
}

type answer struct {
	status     int
	retryAfter string
	body       map[string]any
	err        error
}

func send(url, key, body string) answer {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url+"/api/v1/checkout", bytes.NewBufferString(body))
	if err != nil {
		return answer{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Demo-User-ID", "alice")
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return answer{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return answer{err: err}
	}
	out := answer{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After")}
	out.err = json.Unmarshal(raw, &out.body)
	return out
}

func received(t *testing.T, a answer) answer {
	t.Helper()
	require.NoError(t, a.err)
	return a
}

func TestASameKeyCheckoutWaitsAtMostTheLockTimeoutAndThenGetsTheFirstResult(t *testing.T) {
	pool, _ := dbtest.Start(t)
	ctx := context.Background()
	quiet, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	cat, err := catalog.NewModule(catalog.Deps{Pool: pool, Log: quiet, Metrics: telemetry.Noop(), Tracer: telemetry.Noop(), Import: catalog.ImportLimits{MaxBytes: 1 << 20, MaxRows: 100}})
	require.NoError(t, err)
	real, err := payment.NewModule(pool)
	require.NoError(t, err)
	pay := &gate{real: real, charges: map[string]int{}, deadlines: map[string]time.Time{}}
	barrier := &barrierCatalog{Module: cat, entered: make(chan struct{}), release: make(chan struct{})}
	orders, err := ordering.NewModule(ordering.Deps{
		Pool: pool, Catalog: barrier, Payments: pay, Log: quiet, Metrics: telemetry.Noop(), Users: []string{"alice"},
		Budgets:  ordering.Budgets{Reserve: 10 * time.Second, LockTimeout: 2 * time.Second, Charge: 3 * time.Second, Finalize: 3 * time.Second},
		Recovery: recoveryConfig,
		Events:   eventsConfig,
	})
	require.NoError(t, err)
	router := chi.NewRouter()
	router.Route("/api/v1", orders.RegisterRoutes)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	t.Cleanup(barrier.open)

	price, stock, weight := decimal.RequireFromString("10.00"), int64(5), decimal.RequireFromString("1")
	p, err := cat.Create(ctx, catalog.CreateInput{SKU: "SAME-1", Name: "Same key", Category: "Tests", Price: &price, Stock: &stock, WeightKg: &weight})
	require.NoError(t, err)
	require.NoError(t, orders.AddToCart(ctx, "alice", p.ID, 1))
	count := func(sql string, args ...any) int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, sql, args...).Scan(&n))
		return n
	}
	movements := count(`SELECT count(*) FROM inventory_movements`)
	key := uuid.NewString()
	body := `{"items":[{"product_id":"` + p.ID + `","quantity":1,"expected_unit_price":"10.00"}],"payment_token":"pm_approved"}`

	first := make(chan answer, 1)
	go func() { first <- send(server.URL, key, body) }()
	<-barrier.entered

	second := make(chan answer, 1)
	go func() { second <- send(server.URL, key, body) }()
	require.Eventually(t, func() bool {
		return count(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'INSERT INTO purchases%'`) == 1
	}, 10*time.Second, 10*time.Millisecond, "the second request waits on the uncommitted purchase of the first")

	waited := received(t, <-second)
	require.Equal(t, http.StatusServiceUnavailable, waited.status, waited.body)
	require.Equal(t, "1", waited.retryAfter)
	require.Equal(t, "try_again", waited.body["code"])
	require.Zero(t, count(`SELECT count(*) FROM purchases`), "no purchase is visible: the first is uncommitted and the second rolled back")
	require.Zero(t, count(`SELECT count(*) FROM purchase_items`))
	require.Zero(t, count(`SELECT reserved FROM products WHERE id = $1::uuid`, p.ID))
	require.Equal(t, movements, count(`SELECT count(*) FROM inventory_movements`))
	require.Zero(t, count(`SELECT count(*) FROM payment_fake_charges`))
	require.Equal(t, 1, count(`SELECT quantity FROM cart_items WHERE user_id = 'alice' AND product_id = $1::uuid`, p.ID), "the cart is untouched")

	barrier.open()
	created := received(t, <-first)
	require.Equal(t, http.StatusCreated, created.status, created.body)
	require.Equal(t, "CONFIRMED", created.body["status"], "the first request finished its reservation and its payment")
	id, _ := created.body["id"].(string)
	require.Equal(t, 1, count(`SELECT count(*) FROM purchases WHERE id = $1::uuid AND status = 'CONFIRMED'`, id))

	replayed := received(t, send(server.URL, key, body))
	require.Equal(t, http.StatusOK, replayed.status, replayed.body)
	require.Equal(t, id, replayed.body["id"], "the retry gets the purchase of the first request")
	require.Equal(t, "CONFIRMED", replayed.body["status"])

	require.Equal(t, 1, count(`SELECT count(*) FROM purchases`))
	require.Equal(t, 1, count(`SELECT count(*) FROM purchase_items`))
	require.Equal(t, 1, pay.chargesFor(id), "one charge request, from the first request")
	require.Equal(t, 1, count(`SELECT count(*) FROM payment_fake_charges WHERE reference = $1`, id))
	require.Equal(t, 1, count(`SELECT count(*) FROM inventory_movements WHERE reason = 'reservation' AND reference = $1`, id))
	require.Equal(t, 1, count(`SELECT count(*) FROM inventory_movements WHERE reason = 'sale' AND reference = $1`, id), "the stock was settled once")
	require.Zero(t, count(`SELECT count(*) FROM cart_items WHERE user_id = 'alice'`), "the confirmed first request cleaned the cart")
}
