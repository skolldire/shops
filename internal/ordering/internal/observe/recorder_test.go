package observe_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/ordering/internal/observe"
	"github.com/skolldire/shops/internal/platform/logger"
)

type counter struct {
	name  string
	value int64
	attrs []attribute.KeyValue
}

type counters struct{ got []counter }

func (c *counters) Counter(_ context.Context, name string, value int64, attrs ...attribute.KeyValue) {
	c.got = append(c.got, counter{name, value, attrs})
}

func newRecorder(t *testing.T) (*observe.CheckoutRecorder, *counters, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	c := &counters{}
	r, err := observe.NewCheckoutRecorder(c, log)
	require.NoError(t, err)
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

func TestRecoveryCountsItsOutcome(t *testing.T) {
	r, c, logs := newRecorder(t)
	r.Recovery(context.Background(), "still_pending", 2)
	r.Recovery(context.Background(), "confirmed", 0)
	require.Equal(t, []counter{
		{"checkout_recovery_total", 2, []attribute.KeyValue{attribute.String("outcome", "still_pending")}},
		{"checkout_recovery_total", 0, []attribute.KeyValue{attribute.String("outcome", "confirmed")}},
	}, c.got, "zero is recorded too, so the series exists after a startup without pending purchases")
	require.Empty(t, logs.String())
}

func TestNewCheckoutRecorderRequiresItsDependencies(t *testing.T) {
	_, err := observe.NewCheckoutRecorder(nil, nil)
	require.Error(t, err)
}
