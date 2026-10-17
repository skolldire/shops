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

func newRelayRecorder(t *testing.T) (*observe.RelayRecorder, *counters, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "debug"}, &logs)
	require.NoError(t, err)
	c := &counters{}
	r, err := observe.NewRelayRecorder(c, log)
	require.NoError(t, err)
	return r, c, &logs
}

func TestEveryRelaySeriesExistsFromTheStart(t *testing.T) {
	_, c, _ := newRelayRecorder(t)
	names := map[string]int{}
	for _, got := range c.got {
		require.Zero(t, got.value)
		names[got.name]++
	}
	require.Equal(t, map[string]int{"outbox_relay_total": 4, "inbox_duplicates_total": 1}, names)
}

func relayAttempt(attempts int, outcome string) app.RelayAttempt {
	return app.RelayAttempt{Seq: 7, Type: domain.EventPurchaseConfirmed, Attempts: attempts, Outcome: outcome, Cause: errors.New("projection unavailable"), StuckAfter: 3}
}

func TestAStuckEventIsAnErrorOnceAndAWarningAfterwards(t *testing.T) {
	r, c, logs := newRelayRecorder(t)
	c.got = nil
	ctx := context.Background()
	for _, a := range []app.RelayAttempt{
		relayAttempt(1, app.RelayStillPending), relayAttempt(2, app.RelayStillPending), relayAttempt(3, app.RelayStillPending),
		relayAttempt(4, app.RelayStillPending), relayAttempt(4, app.RelayPublished), relayAttempt(0, app.RelaySkipped),
		relayAttempt(0, app.RelayError),
	} {
		r.RelayAttempt(ctx, a)
	}
	r.RelayDuplicate(ctx)
	r.RelayGauges(ctx, app.EventStats{Pending: 2, OldestSeconds: 4.5, Stuck: 1, Unsupported: 3})

	out := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, out, 7)
	severities := make([]string, len(out))
	for i, line := range out {
		for _, s := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
			if strings.Contains(line, `"severity":"`+s+`"`) {
				severities[i] = s
			}
		}
		require.Contains(t, line, `"event_seq":7`)
		require.NotContains(t, line, "payload")
	}
	require.Equal(t, []string{"INFO", "INFO", "ERROR", "WARN", "DEBUG", "INFO", "WARN"}, severities)
	require.Contains(t, out[2], `"attempt":3`)
	require.Contains(t, out[2], "projection unavailable")
	require.Equal(t, attribute.String("outcome", "still_pending"), c.got[0].attrs[0])
	require.Equal(t, map[string]float64{"outbox_pending_events": 2, "outbox_oldest_pending_seconds": 4.5, "outbox_stuck_events": 1, "outbox_unsupported_events": 3}, c.gauges)
	require.Equal(t, "inbox_duplicates_total", c.got[len(c.got)-1].name)
}

func TestNewRelayRecorderRequiresItsDependencies(t *testing.T) {
	_, err := observe.NewRelayRecorder(nil, nil)
	require.Error(t, err)
}
