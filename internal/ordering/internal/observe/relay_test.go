package observe_test

import (
	"bytes"
	"context"
	"errors"
	"math"
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

func severities(t *testing.T, logs *bytes.Buffer) []string {
	t.Helper()
	out := strings.Split(strings.TrimSpace(logs.String()), "\n")
	got := make([]string, len(out))
	for i, line := range out {
		for _, s := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
			if strings.Contains(line, `"severity":"`+s+`"`) {
				got[i] = s
			}
		}
		require.Contains(t, line, `"event_seq":7`)
		require.NotContains(t, line, "payload")
	}
	return got
}

func TestAStuckEventIsAnErrorOnceAndAWarningAtEveryDoubling(t *testing.T) {
	r, c, logs := newRelayRecorder(t)
	c.got = nil
	ctx := context.Background()
	for _, a := range []app.RelayAttempt{
		relayAttempt(1, app.RelayStillPending), relayAttempt(2, app.RelayStillPending), relayAttempt(3, app.RelayStillPending),
		relayAttempt(4, app.RelayStillPending), relayAttempt(6, app.RelayStillPending), relayAttempt(7, app.RelayStillPending),
		relayAttempt(12, app.RelayStillPending), relayAttempt(12, app.RelayPublished), relayAttempt(0, app.RelaySkipped),
		relayAttempt(0, app.RelayError),
	} {
		r.RelayAttempt(ctx, a)
	}
	r.RelayDuplicate(ctx)
	r.RelayGauges(ctx, app.EventStats{Pending: 2, OldestSeconds: 4.5, Stuck: 1, Unsupported: 3})

	got := severities(t, logs)
	require.Equal(t, []string{"INFO", "INFO", "ERROR", "DEBUG", "WARN", "DEBUG", "WARN", "DEBUG", "INFO", "WARN"}, got)
	out := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Contains(t, out[2], `"attempt":3`)
	require.Contains(t, out[2], "projection unavailable")
	stillPending := int64(0)
	for _, got := range c.got {
		if got.name == "outbox_relay_total" && len(got.attrs) == 1 && got.attrs[0] == attribute.String("outcome", app.RelayStillPending) {
			stillPending += got.value
		}
	}
	require.Equal(t, int64(7), stillPending, "every attempt is counted, logged or not")
	require.Equal(t, map[string]float64{"outbox_pending_events": 2, "outbox_oldest_pending_seconds": 4.5, "outbox_stuck_events": 1, "outbox_unsupported_events": 3}, c.gauges)
	require.Equal(t, "inbox_duplicates_total", c.got[len(c.got)-1].name)
}

func TestTheWarningPointsDoubleTheThresholdWithoutOverflow(t *testing.T) {
	for _, tc := range []struct {
		threshold int
		warns     []int
	}{
		{10, []int{20, 40, 80}},
		{3, []int{6, 12, 24, 48}},
		{1, []int{2, 4, 8, 16, 32, 64}},
	} {
		var got []int
		for attempts := tc.threshold + 1; attempts <= tc.warns[len(tc.warns)-1]+1; attempts++ {
			r, _, logs := newRelayRecorder(t)
			r.RelayAttempt(context.Background(), app.RelayAttempt{Seq: 7, Attempts: attempts, Outcome: app.RelayStillPending, StuckAfter: tc.threshold})
			if strings.Contains(logs.String(), `"severity":"WARN"`) {
				got = append(got, attempts)
			}
		}
		require.Equal(t, tc.warns, got, "threshold %d", tc.threshold)
	}
	r, _, logs := newRelayRecorder(t)
	for _, a := range []app.RelayAttempt{
		{Seq: 7, Attempts: math.MaxInt, Outcome: app.RelayStillPending, StuckAfter: math.MaxInt/2 + 1},
		{Seq: 7, Attempts: math.MaxInt - 1, Outcome: app.RelayStillPending, StuckAfter: math.MaxInt / 2},
		{Seq: 7, Attempts: math.MaxInt, Outcome: app.RelayStillPending, StuckAfter: 1},
	} {
		r.RelayAttempt(context.Background(), a)
	}
	require.Equal(t, []string{"DEBUG", "WARN", "DEBUG"}, severities(t, logs), "large attempts and thresholds never overflow")
}

func TestNewRelayRecorderRequiresItsDependencies(t *testing.T) {
	_, err := observe.NewRelayRecorder(nil, nil)
	require.Error(t, err)
}
