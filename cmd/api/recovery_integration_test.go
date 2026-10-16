//go:build integration

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/lifecycle"
	"github.com/skolldire/shops/internal/platform/logger"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestStartRecoversPendingPurchasesBeforeServing(t *testing.T) {
	c := startApp(t)
	cfg := c.cfg
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Metrics.Addr = "127.0.0.1:0"
	cfg.Demo.Seed = false

	id := c.product("REC-1", "8.00", 5)
	resp, body := c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(id, 2, "8.00")))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	purchase := decodePurchase(t, body)
	_, err := c.pool.Exec(context.Background(),
		`UPDATE purchases SET status = 'PENDING', payment_ref = NULL WHERE id = $1::uuid`, purchase.ID)
	require.NoError(t, err)
	_, err = c.pool.Exec(context.Background(), `UPDATE products SET stock = stock + 2, reserved = reserved + 2 WHERE id = $1::uuid`, id)
	require.NoError(t, err)

	t.Run("a database error stops the startup", func(t *testing.T) {
		_, err := c.pool.Exec(context.Background(), `ALTER TABLE purchases RENAME COLUMN payment_token TO payment_token_hidden`)
		require.NoError(t, err)
		defer func() {
			_, err := c.pool.Exec(context.Background(), `ALTER TABLE purchases RENAME COLUMN payment_token_hidden TO payment_token`)
			require.NoError(t, err)
		}()
		var logs syncBuffer
		log, err := logger.New(logger.Config{Level: "info"}, &logs)
		require.NoError(t, err)
		lc, err := lifecycle.New(log)
		require.NoError(t, err)

		err = start(t.Context(), cfg, log, lc)

		require.ErrorContains(t, err, "purchase recovery: ordering: list pending purchases")
		require.NoError(t, lc.Close(context.Background()))
		require.NotContains(t, logs.String(), "server listening")
	})

	t.Run("pending purchases are settled before the server listens", func(t *testing.T) {
		var logs syncBuffer
		log, err := logger.New(logger.Config{Level: "info"}, &logs)
		require.NoError(t, err)
		lc, err := lifecycle.New(log)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- start(ctx, cfg, log, lc) }()

		require.Eventually(t, func() bool { return strings.Contains(logs.String(), "server listening") }, 20*time.Second, 20*time.Millisecond)
		out := logs.String()
		recovered := strings.Index(out, `"message":"pending purchases recovered"`)
		require.GreaterOrEqual(t, recovered, 0)
		require.Less(t, recovered, strings.Index(out, "server listening"))
		require.Contains(t, out, `"confirmed":1`)
		require.Contains(t, out, `"still_pending":0`)
		require.NotContains(t, out, "pm_approved")

		var status, ref string
		require.NoError(t, c.pool.QueryRow(context.Background(),
			`SELECT status, payment_ref FROM purchases WHERE id = $1::uuid`, purchase.ID).Scan(&status, &ref))
		require.Equal(t, "CONFIRMED", status)
		require.Equal(t, "fake_"+purchase.ID, ref)
		require.Equal(t, 3, c.stockOf(id), "the reservation made before the interruption becomes the sale")
		require.Zero(t, c.scalar(`SELECT reserved FROM products WHERE id = $1::uuid`, id))

		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("the app did not stop")
		}
		require.NoError(t, lc.Close(context.Background()))
	})
}

func TestCheckoutMetricsAreExposed(t *testing.T) {
	c := startApp(t)
	_, err := c.app.ordering.RecoverPending(t.Context())
	require.NoError(t, err)
	id := c.product("MET-1", "2.00", 5)
	resp, _ := c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(id, 1, "2.00")))
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	rec := httptest.NewRecorder()
	c.app.metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	require.Contains(t, body, `checkout_total{outcome="confirmed"} 1`)
	require.Contains(t, body, `checkout_recovery_total{outcome="still_pending"} 0`)
	require.Contains(t, body, `checkout_recovery_total{outcome="confirmed"} 0`)
	require.NotContains(t, body, "alice")
	require.NotContains(t, body, "pm_approved")
}
