package health_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/health"
	"github.com/skolldire/shops/internal/platform/logger"
)

func ok() health.Checker {
	return health.CheckerFunc(func(context.Context) error { return nil })
}

func failing(msg string) health.Checker {
	return health.CheckerFunc(func(context.Context) error { return errors.New(msg) })
}

func blocking() health.Checker {
	return health.CheckerFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
}

func newService(t *testing.T, timeout time.Duration) (*health.Service, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "debug"}, &logs)
	require.NoError(t, err)
	svc, err := health.New(health.Config{Timeout: timeout}, log)
	require.NoError(t, err)
	return svc, &logs
}

func TestCheck(t *testing.T) {
	tests := map[string]struct {
		checkers map[string]health.Checker
		want     health.Report
	}{
		"no checkers": {nil, health.Report{Status: "up", Checks: map[string]string{}}},
		"all up":      {map[string]health.Checker{"postgres": ok(), "cache": ok()}, health.Report{Status: "up", Checks: map[string]string{"postgres": "up", "cache": "up"}}},
		"one down":    {map[string]health.Checker{"postgres": failing("refused"), "cache": ok()}, health.Report{Status: "down", Checks: map[string]string{"postgres": "down", "cache": "up"}}},
		"timeout":     {map[string]health.Checker{"postgres": blocking()}, health.Report{Status: "down", Checks: map[string]string{"postgres": "down"}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			svc, _ := newService(t, 50*time.Millisecond)
			for n, c := range tt.checkers {
				svc.Register(n, c)
			}

			require.Equal(t, tt.want, svc.Check(t.Context()))
		})
	}
}

func TestCheckLogsFailureDetail(t *testing.T) {
	svc, logs := newService(t, time.Second)
	svc.Register("postgres", failing("dial tcp 10.0.0.5:5432: connection refused"))

	svc.Check(t.Context())

	require.Contains(t, logs.String(), `"message":"health check failed"`)
	require.Contains(t, logs.String(), `"check":"postgres"`)
	require.Contains(t, logs.String(), "connection refused")
}

func TestCheckRunsInParallel(t *testing.T) {
	slow := health.CheckerFunc(func(ctx context.Context) error {
		select {
		case <-time.After(100 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	svc, _ := newService(t, time.Second)
	svc.Register("a", slow).Register("b", slow).Register("c", slow)

	start := time.Now()
	report := svc.Check(t.Context())

	require.Equal(t, "up", report.Status)
	require.Less(t, time.Since(start), 250*time.Millisecond)
}

func TestNewValidates(t *testing.T) {
	log, err := logger.New(logger.Config{Level: "info"}, io.Discard)
	require.NoError(t, err)

	_, err = health.New(health.Config{}, log)
	require.EqualError(t, err, "health: timeout must be positive")
	_, err = health.New(health.Config{Timeout: time.Second}, nil)
	require.EqualError(t, err, "health: logger is required")
}
