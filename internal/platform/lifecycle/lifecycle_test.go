package lifecycle_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/lifecycle"
	"github.com/skolldire/shops/internal/platform/logger"
)

func newLifecycle(t *testing.T) (*lifecycle.Lifecycle, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "debug"}, &logs)
	require.NoError(t, err)
	lc, err := lifecycle.New(log)
	require.NoError(t, err)
	return lc, &logs
}

func TestCloseRunsInReverseOrder(t *testing.T) {
	lc, logs := newLifecycle(t)
	var order []string
	for _, name := range []string{"postgres", "telemetry", "worker"} {
		require.NoError(t, lc.OnClose(name, func(context.Context) error {
			order = append(order, name)
			return nil
		}))
	}

	require.NoError(t, lc.Close(t.Context()))

	require.Equal(t, []string{"worker", "telemetry", "postgres"}, order)
	require.Contains(t, logs.String(), `"component":"postgres"`)
}

func TestCloseAggregatesErrorsAndContinues(t *testing.T) {
	lc, _ := newLifecycle(t)
	e1, e2 := errors.New("e1"), errors.New("e2")
	ran := false
	require.NoError(t, lc.OnClose("a", func(context.Context) error { ran = true; return nil }))
	require.NoError(t, lc.OnClose("b", func(context.Context) error { return e1 }))
	require.NoError(t, lc.OnClose("c", func(context.Context) error { return e2 }))

	err := lc.Close(t.Context())

	require.ErrorIs(t, err, e1)
	require.ErrorIs(t, err, e2)
	require.ErrorContains(t, err, "lifecycle: close b: e1")
	require.True(t, ran)
}

func TestCloseIsIdempotentAndRejectsLateRegistrations(t *testing.T) {
	lc, _ := newLifecycle(t)
	calls := 0
	require.NoError(t, lc.OnClose("a", func(context.Context) error { calls++; return nil }))

	require.NoError(t, lc.Close(t.Context()))
	require.NoError(t, lc.Close(t.Context()))

	require.Equal(t, 1, calls)
	require.ErrorIs(t, lc.OnClose("late", func(context.Context) error { return nil }), lifecycle.ErrClosed)
}

func TestValidation(t *testing.T) {
	_, err := lifecycle.New(nil)
	require.EqualError(t, err, "lifecycle: logger is required")

	lc, _ := newLifecycle(t)
	require.EqualError(t, lc.OnClose("x", nil), "lifecycle: closer x is nil")
}
