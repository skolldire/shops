package lifecycle_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func server(addr, body string) *http.Server {
	return &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})}
}

func get(t *testing.T, addr string) string {
	t.Helper()
	var resp *http.Response
	require.Eventually(t, func() bool {
		var err error
		resp, err = http.Get("http://" + addr)
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func TestServeAndGracefulShutdown(t *testing.T) {
	lc, logs := newLifecycle(t)
	closed := false
	require.NoError(t, lc.OnClose("postgres", func(context.Context) error { closed = true; return nil }))
	apiAddr, metricsAddr := freeAddr(t), freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- lc.Serve(ctx, time.Second, server(apiAddr, "api"), server(metricsAddr, "metrics")) }()

	require.Equal(t, "api", get(t, apiAddr))
	require.Equal(t, "metrics", get(t, metricsAddr))
	cancel()
	require.NoError(t, <-done)
	require.True(t, closed)
	_, err := http.Get("http://" + apiAddr)
	require.Error(t, err)
	require.Contains(t, logs.String(), "shutdown requested")
	require.Contains(t, logs.String(), "shutdown complete")
}

func TestServeFailsFastWhenPortIsBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer busy.Close()
	lc, _ := newLifecycle(t)
	closed := false
	require.NoError(t, lc.OnClose("postgres", func(context.Context) error { closed = true; return nil }))
	free := freeAddr(t)

	err = lc.Serve(t.Context(), time.Second, server(free, "ok"), server(busy.Addr().String(), "x"))

	require.ErrorContains(t, err, "lifecycle: listen "+busy.Addr().String())
	require.True(t, closed)
	ln, err := net.Listen("tcp", free)
	require.NoError(t, err, "listener of the first server must be released")
	require.NoError(t, ln.Close())
}

func TestShutdownTimeoutBoundsSlowRequests(t *testing.T) {
	lc, _ := newLifecycle(t)
	addr := freeAddr(t)
	started := make(chan struct{})
	slow := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		time.Sleep(5 * time.Second)
	})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- lc.Serve(ctx, 100*time.Millisecond, slow) }()
	go func() {
		for {
			if _, err := http.Get("http://" + addr); err == nil {
				return
			}
			select {
			case <-started:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	<-started
	start := time.Now()
	cancel()

	require.ErrorIs(t, <-done, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
}
