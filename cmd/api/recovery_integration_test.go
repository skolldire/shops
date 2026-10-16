//go:build integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
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

func (c client) agePending(by string) {
	c.t.Helper()
	ctx := context.Background()
	tx, err := c.pool.Begin(ctx)
	require.NoError(c.t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
	require.NoError(c.t, err)
	_, err = tx.Exec(ctx, `UPDATE purchases SET created_at = now() - $1::interval, next_attempt_at = now() WHERE status = 'PENDING'`, by)
	require.NoError(c.t, err)
	require.NoError(c.t, tx.Commit(ctx))
}

func TestTheWorkerRecoversALostPaymentAnswerWithoutARestart(t *testing.T) {
	c := startApp(t)
	cfg := c.cfg
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Metrics.Addr = "127.0.0.1:0"
	cfg.Demo.Seed = false
	cfg.Recovery.Interval = 20 * time.Millisecond

	var logs syncBuffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	lc, err := lifecycle.New(log)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- start(ctx, cfg, log, lc) }()
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), "server listening") }, 20*time.Second, 20*time.Millisecond)
	require.Contains(t, logs.String(), "recovery worker started", "the worker starts with the servers, without blocking them")

	id := c.product("REC-1", "8.00", 5)
	resp, body := c.checkout("alice", uuid.NewString(), checkoutBody("pm_unknown", item(id, 2, "8.00")))
	require.Equal(t, http.StatusAccepted, resp.StatusCode, body)
	purchase := decodePurchase(t, body)
	require.Equal(t, "PENDING", purchase.Status)

	c.agePending("1 minute")
	require.Eventually(t, func() bool {
		return c.scalar(`SELECT count(*) FROM purchases WHERE id = $1::uuid AND status = 'CONFIRMED'`, purchase.ID) == 1
	}, 20*time.Second, 20*time.Millisecond, "the running worker confirms it")
	require.Equal(t, 3, c.stockOf(id), "the reservation became the sale")
	require.Zero(t, c.scalar(`SELECT reserved FROM products WHERE id = $1::uuid`, id))
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM payment_fake_charges WHERE reference = $1`, purchase.ID), "charged once")
	require.NotContains(t, logs.String(), "pm_unknown")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the app did not stop")
	}
	require.NoError(t, lc.Close(context.Background()))
	out := logs.String()
	require.Less(t, strings.Index(out, `"component":"recovery"`), strings.Index(out, `"component":"postgres"`), "the worker stops before the pool")
}

func TestCheckoutMetricsAreExposed(t *testing.T) {
	c := startApp(t)
	_, err := c.app.ordering.RecoverOnce(t.Context())
	require.NoError(t, err)
	id := c.product("MET-1", "2.00", 5)
	resp, _ := c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(id, 1, "2.00")))
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	rec := httptest.NewRecorder()
	c.app.metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	require.Contains(t, body, `checkout_total{outcome="confirmed"} 1`)
	require.Contains(t, body, `checkout_recovery_total{outcome="still_pending"} 0`)
	require.Contains(t, body, `checkout_recovery_resolutions_total{result="voided"} 0`)
	for _, gauge := range []string{"checkout_pending_purchases", "checkout_oldest_pending_seconds", "checkout_recovery_stuck", "checkout_recovery_stuck_reserved_units"} {
		require.Contains(t, body, gauge)
	}
	require.NotContains(t, body, "alice")
	require.NotContains(t, body, "pm_approved")
}

func listening(t *testing.T, logs string) string {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(logs))
	for scanner.Scan() {
		var line struct {
			Message string `json:"message"`
			Addr    string `json:"addr"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Addr != "" && strings.Contains(scanner.Text(), "server listening") {
			return line.Addr
		}
	}
	t.Fatal("the api server did not log its address")
	return ""
}

func TestShutdownStopsTheWorkerWhileHTTPDrainsAndBeforeThePool(t *testing.T) {
	c := startApp(t)
	cfg := c.cfg
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Metrics.Addr = "127.0.0.1:0"
	cfg.Demo.Seed = false
	cfg.Recovery.Interval = 20 * time.Millisecond

	var logs syncBuffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	lc, err := lifecycle.New(log)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- start(ctx, cfg, log, lc) }()
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), "server listening") }, 20*time.Second, 20*time.Millisecond)

	conn, err := net.Dial("tcp", listening(t, logs.String()))
	require.NoError(t, err)
	_, err = conn.Write([]byte("POST /api/v1/products HTTP/1.1\r\nHost: shop\r\nContent-Type: application/json\r\nContent-Length: 64\r\nExpect: 100-continue\r\n\r\n"))
	require.NoError(t, err)
	status, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "HTTP/1.1 100 Continue\r\n", status, "the handler is reading the body, so the request is in flight")

	cancel()
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), "recovery worker stopped") }, 10*time.Second, 10*time.Millisecond,
		"the worker stops as soon as the shutdown starts")
	out := logs.String()
	require.Contains(t, out, "shutdown requested")
	require.NotContains(t, out, "shutdown complete", "the request in flight is still being drained")
	require.NotContains(t, out, `"component":"postgres"`, "the pool is still open")

	require.NoError(t, conn.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the app did not stop")
	}
	out = logs.String()
	stopped := strings.Index(out, "recovery worker stopped")
	require.Less(t, stopped, strings.Index(out, `"component":"recovery"`))
	require.Less(t, strings.Index(out, `"component":"recovery"`), strings.Index(out, `"component":"postgres"`), "the worker stops before the pool")
}
