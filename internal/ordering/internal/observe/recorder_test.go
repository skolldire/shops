package observe_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/ordering/internal/observe"
	"github.com/skolldire/shops/internal/platform/logger"
)

type counter struct {
	name  string
	value int64
	attrs []attribute.KeyValue
}

type counters struct {
	got    []counter
	gauges map[string]float64
}

func (c *counters) Counter(_ context.Context, name string, value int64, attrs ...attribute.KeyValue) {
	c.got = append(c.got, counter{name, value, attrs})
}

func (c *counters) Gauge(_ context.Context, name string, value float64, _ ...attribute.KeyValue) {
	if c.gauges == nil {
		c.gauges = map[string]float64{}
	}
	c.gauges[name] = value
}

func newRecorder(t *testing.T) (*observe.CheckoutRecorder, *counters, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	c := &counters{}
	r, err := observe.NewCheckoutRecorder(c, log)
	require.NoError(t, err)
	c.got = nil
	return r, c, &logs
}

func lines(buf *bytes.Buffer) []string {
	return strings.Split(strings.TrimSpace(buf.String()), "\n")
}

func TestCheckoutCountsOneOutcomeAndLogsOneLineWithoutTheToken(t *testing.T) {
	r, c, logs := newRecorder(t)
	p := &domain.Purchase{ID: "p-1", UserID: "alice", Status: domain.StatusConfirmed, PaymentToken: "pm_secret_token_value"}
	r.Checkout(context.Background(), "confirmed", p, nil)

	require.Equal(t, []counter{{"checkout_total", 1, []attribute.KeyValue{attribute.String("outcome", "confirmed")}}}, c.got)
	out := lines(logs)
	require.Len(t, out, 1)
	require.Contains(t, out[0], `"purchase_id":"p-1"`)
	require.Contains(t, out[0], `"user_id":"alice"`)
	require.Contains(t, out[0], `"status":"CONFIRMED"`)
	require.NotContains(t, logs.String(), "pm_secret_token_value")
}

func TestCheckoutWithoutAPurchaseOnlyCounts(t *testing.T) {
	r, c, logs := newRecorder(t)
	r.Checkout(context.Background(), "invalid", nil, nil)
	require.Len(t, c.got, 1)
	require.Empty(t, logs.String())
}

func TestPendingCheckoutLogsItsCauseOnceAsAWarning(t *testing.T) {
	r, _, logs := newRecorder(t)
	p := &domain.Purchase{ID: "p-2", UserID: "bob", Status: domain.StatusPending, PaymentToken: "pm_approved"}
	r.Checkout(context.Background(), "pending", p, errors.New("ordering: charge: timeout"))
	out := lines(logs)
	require.Len(t, out, 1)
	require.Contains(t, out[0], `"severity":"WARN"`)
	require.Contains(t, out[0], `"cause":"ordering: charge: timeout"`)
	require.NotContains(t, out[0], "pm_approved")
}

func TestEveryRecoverySeriesExistsFromTheStart(t *testing.T) {
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	c := &counters{}
	_, err = observe.NewCheckoutRecorder(c, log)
	require.NoError(t, err)
	names := map[string]int{}
	for _, got := range c.got {
		require.Zero(t, got.value)
		names[got.name]++
	}
	require.Equal(t, map[string]int{"checkout_recovery_total": 5, "checkout_recovery_resolutions_total": 5}, names,
		"zero is recorded, so the series exist before the first attempt")
}

func attempt(attempts int, outcome string) app.RecoveryAttempt {
	return app.RecoveryAttempt{PurchaseID: "p-1", Attempts: attempts, Outcome: outcome, Cause: errors.New("ordering: charge: unavailable"), StuckAfter: 3}
}

func TestAStuckPurchaseIsAnErrorOnceAndAWarningAfterwards(t *testing.T) {
	r, c, logs := newRecorder(t)
	ctx := context.Background()
	r.RecoveryAttempt(ctx, attempt(1, app.RecoveryStillPending))
	r.RecoveryAttempt(ctx, attempt(2, app.RecoveryStillPending))
	r.RecoveryAttempt(ctx, attempt(3, app.RecoveryStillPending))
	r.RecoveryAttempt(ctx, attempt(4, app.RecoveryStillPending))
	r.RecoveryAttempt(ctx, attempt(5, app.RecoveryError))
	r.RecoveryAttempt(ctx, attempt(5, app.RecoveryConfirmed))
	r.RecoveryAttempt(ctx, app.RecoveryAttempt{PurchaseID: "p-2", Outcome: app.RecoverySkipped, StuckAfter: 3})

	out := lines(logs)
	require.Len(t, out, 7, "one line per attempt")
	severities := make([]string, len(out))
	for i, line := range out {
		for _, s := range []string{"INFO", "WARN", "ERROR"} {
			if strings.Contains(line, `"severity":"`+s+`"`) {
				severities[i] = s
			}
		}
		require.Contains(t, line, `"purchase_id":"p-`)
	}
	require.Equal(t, []string{"INFO", "INFO", "ERROR", "WARN", "WARN", "INFO", "INFO"}, severities)
	require.Contains(t, out[2], `"attempt":3`)
	require.Contains(t, out[2], `"cause":"ordering: charge: unavailable"`)
	require.Contains(t, out[5], "a stuck purchase was resolved")
	require.Contains(t, out[6], "skipped")
	require.Len(t, c.got, 7)
	require.Equal(t, attribute.String("outcome", "still_pending"), c.got[0].attrs[0])
}

func TestResolutionsAreCountedAndAMismatchIsAnError(t *testing.T) {
	r, c, logs := newRecorder(t)
	ctx := context.Background()
	r.RecoveryResolution(ctx, app.RecoveryResolution{PurchaseID: "p-1", Attempts: 10, Result: app.ResolutionVoided})
	r.RecoveryResolution(ctx, app.RecoveryResolution{PurchaseID: "p-2", Attempts: 10, Result: app.ResolutionMismatch, Cause: errors.New("1.00 USD instead of 9.99 USD")})

	require.Equal(t, []counter{
		{"checkout_recovery_resolutions_total", 1, []attribute.KeyValue{attribute.String("result", "voided")}},
		{"checkout_recovery_resolutions_total", 1, []attribute.KeyValue{attribute.String("result", "mismatch")}},
	}, c.got)
	out := lines(logs)
	require.Contains(t, out[0], `"severity":"INFO"`)
	require.Contains(t, out[0], `"result":"voided"`)
	require.Contains(t, out[1], `"severity":"ERROR"`)
}

func TestRecoveryGauges(t *testing.T) {
	r, c, _ := newRecorder(t)
	r.RecoveryGauges(context.Background(), app.PendingStats{Pending: 4, OldestSeconds: 75.5, Stuck: 2, StuckReservedUnits: 7})
	require.Equal(t, map[string]float64{
		"checkout_pending_purchases": 4, "checkout_oldest_pending_seconds": 75.5,
		"checkout_recovery_stuck": 2, "checkout_recovery_stuck_reserved_units": 7,
	}, c.gauges)
}

func TestNewCheckoutRecorderRequiresItsDependencies(t *testing.T) {
	_, err := observe.NewCheckoutRecorder(nil, nil)
	require.Error(t, err)
}
