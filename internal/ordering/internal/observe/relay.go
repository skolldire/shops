package observe

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/platform/logger"
)

const (
	relayTotal          = "outbox_relay_total"
	inboxDuplicates     = "inbox_duplicates_total"
	pendingEventsGauge  = "outbox_pending_events"
	oldestEventGauge    = "outbox_oldest_pending_seconds"
	stuckEventsGauge    = "outbox_stuck_events"
	relayDeliveredEvent = "ordering: an outbox event was delivered"
)

var errEventStuck = errors.New("ordering: an outbox event is stuck and keeps being retried")

type RelayRecorder struct {
	counters Counters
	log      logger.Service
}

func NewRelayRecorder(counters Counters, log logger.Service) (*RelayRecorder, error) {
	if counters == nil || log == nil {
		return nil, errors.New("ordering: relay recorder requires counters and a logger")
	}
	ctx := context.Background()
	for _, outcome := range []string{app.RelayPublished, app.RelayStillPending, app.RelaySkipped, app.RelayError} {
		counters.Counter(ctx, relayTotal, 0, attribute.String(outcomeKey, outcome))
	}
	counters.Counter(ctx, inboxDuplicates, 0)
	return &RelayRecorder{counters: counters, log: log}, nil
}

func (r *RelayRecorder) RelayAttempt(ctx context.Context, a app.RelayAttempt) {
	r.counters.Counter(ctx, relayTotal, 1, attribute.String(outcomeKey, a.Outcome))
	fields := map[string]any{"component": component, "event_seq": a.Seq, "event_type": string(a.Type), "attempt": a.Attempts, "outcome": a.Outcome}
	if a.Cause != nil {
		fields["cause"] = a.Cause.Error()
	}
	switch {
	case a.Outcome == app.RelayPublished:
		r.log.Debug(ctx, relayDeliveredEvent, fields)
	case a.Outcome == app.RelaySkipped:
		r.log.Info(ctx, "ordering: the relay skipped an event another replica took or published", fields)
	case a.Outcome == app.RelayStillPending && a.Attempts == a.StuckAfter:
		r.log.Error(ctx, errEventStuck, fields)
	case a.Outcome == app.RelayStillPending && a.Attempts > a.StuckAfter:
		r.log.Warn(ctx, "ordering: a stuck outbox event is still pending", fields)
	case a.Outcome == app.RelayStillPending:
		r.log.Info(ctx, "ordering: the relay left an event pending", fields)
	default:
		r.log.Warn(ctx, "ordering: a relay step failed and the event stays pending", fields)
	}
}

func (r *RelayRecorder) RelayDuplicate(ctx context.Context) {
	r.counters.Counter(ctx, inboxDuplicates, 1)
}

func (r *RelayRecorder) RelayGauges(ctx context.Context, s app.EventStats) {
	r.counters.Gauge(ctx, pendingEventsGauge, float64(s.Pending))
	r.counters.Gauge(ctx, oldestEventGauge, s.OldestSeconds)
	r.counters.Gauge(ctx, stuckEventsGauge, float64(s.Stuck))
}
