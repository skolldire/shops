package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

const (
	RelayPublished    = "published"
	RelayStillPending = "still_pending"
	RelaySkipped      = "skipped"
	RelayError        = "error"
)

type RelayAttempt struct {
	Seq        int64
	Type       domain.EventType
	Attempts   int
	Outcome    string
	Cause      error
	StuckAfter int
}

type RelayRecorder interface {
	RelayAttempt(ctx context.Context, a RelayAttempt)
	RelayDuplicate(ctx context.Context)
	RelayGauges(ctx context.Context, s EventStats)
}

type RelayPolicy struct {
	Owner          string
	Lease          time.Duration
	DBTimeout      time.Duration
	HandlerTimeout time.Duration
	Batch          int
	StuckAfter     int
	Backoff        func(attempts int) time.Duration
}

type RelayReport struct {
	Published    int
	Duplicates   int
	Skipped      int
	StillPending int
}

type Relay struct {
	queue   EventQueue
	handler EventHandler
	rec     RelayRecorder
	policy  RelayPolicy
	item    time.Duration
}

func EventItemBudget(dbTimeout, handlerTimeout time.Duration) time.Duration {
	renew, settle := dbTimeout, dbTimeout
	return renew + handlerTimeout + settle
}

func NewRelay(queue EventQueue, handler EventHandler, rec RelayRecorder, policy RelayPolicy) (*Relay, error) {
	item := EventItemBudget(policy.DBTimeout, policy.HandlerTimeout)
	switch {
	case queue == nil, handler == nil, rec == nil:
		return nil, errors.New("ordering: the relay requires an event queue, a handler and a recorder")
	case policy.Owner == "", policy.Backoff == nil:
		return nil, errors.New("ordering: the relay requires an owner and a backoff")
	case policy.Lease <= 0, policy.DBTimeout <= 0, policy.HandlerTimeout <= 0, policy.Batch < 1, policy.StuckAfter < 1:
		return nil, errors.New("ordering: the relay requires a positive lease, database timeout, handler timeout, batch and threshold")
	case policy.Lease <= item+LeaseMargin:
		return nil, errors.New("ordering: the event lease must exceed the deadline of one event plus the margin")
	}
	return &Relay{queue: queue, handler: handler, rec: rec, policy: policy, item: item}, nil
}

func (r *Relay) RunCycle(ctx context.Context) (RelayReport, error) {
	claimCtx, cancel := context.WithTimeout(ctx, r.policy.DBTimeout)
	events, err := r.queue.Claim(claimCtx, r.policy.Owner, r.policy.Lease, r.policy.Batch)
	cancel()
	if err != nil {
		return RelayReport{}, fmt.Errorf("ordering: claim outbox events: %w", err)
	}
	var report RelayReport
	for _, e := range events {
		if ctx.Err() != nil {
			break
		}
		r.relay(ctx, e, &report)
	}
	if ctx.Err() != nil {
		return report, nil
	}
	statsCtx, cancel := context.WithTimeout(ctx, r.policy.DBTimeout)
	defer cancel()
	stats, err := r.queue.Stats(statsCtx, r.policy.StuckAfter)
	if err != nil {
		return report, fmt.Errorf("ordering: the outbox gauges were not updated: %w", err)
	}
	r.rec.RelayGauges(ctx, stats)
	return report, nil
}

func (r *Relay) relay(ctx context.Context, e domain.StoredEvent, report *RelayReport) {
	start := time.Now()
	item, cancelItem := context.WithDeadline(context.WithoutCancel(ctx), start.Add(r.item))
	defer cancelItem()
	renewCtx, cancel := context.WithDeadline(ctx, start.Add(r.policy.DBTimeout))
	owned, err := r.queue.Renew(renewCtx, e.Seq, r.policy.Owner, r.policy.Lease)
	cancel()
	switch {
	case err != nil && ctx.Err() != nil:
		return
	case err != nil:
		report.StillPending++
		r.attempt(ctx, e, e.Attempts, RelayError, err)
		return
	case !owned:
		report.Skipped++
		r.attempt(ctx, e, e.Attempts, RelaySkipped, nil)
		return
	}
	applied, err := r.handle(item, e)
	if err != nil {
		r.reschedule(item, e, err, report)
		return
	}
	if !applied {
		report.Duplicates++
		r.rec.RelayDuplicate(item)
	}
	markCtx, cancel, err := within(item, r.policy.DBTimeout)
	if err == nil {
		owned, err = r.queue.MarkPublished(markCtx, e.Seq, r.policy.Owner)
		cancel()
	}
	switch {
	case err != nil:
		report.StillPending++
		r.attempt(item, e, e.Attempts, RelayError, err)
	case !owned:
		report.Skipped++
		r.attempt(item, e, e.Attempts, RelaySkipped, nil)
	default:
		report.Published++
		r.attempt(item, e, e.Attempts, RelayPublished, nil)
	}
}

func (r *Relay) handle(item context.Context, e domain.StoredEvent) (bool, error) {
	ctx, cancel, err := within(item, r.policy.HandlerTimeout)
	if err != nil {
		return false, err
	}
	defer cancel()
	return r.handler.Handle(ctx, e)
}

func (r *Relay) reschedule(item context.Context, e domain.StoredEvent, cause error, report *RelayReport) {
	report.StillPending++
	var (
		total int
		owned bool
	)
	ctx, cancel, err := within(item, r.policy.DBTimeout)
	if err == nil {
		total, owned, err = r.queue.Reschedule(ctx, e.Seq, r.policy.Owner, r.policy.Backoff(e.Attempts), sanitize(cause))
		cancel()
	}
	switch {
	case err != nil:
		r.attempt(item, e, e.Attempts, RelayError, errors.Join(cause, err))
	case !owned:
		r.attempt(item, e, e.Attempts, RelaySkipped, cause)
	default:
		r.attempt(item, e, total, RelayStillPending, cause)
	}
}

func (r *Relay) attempt(ctx context.Context, e domain.StoredEvent, attempts int, outcome string, cause error) {
	r.rec.RelayAttempt(ctx, RelayAttempt{Seq: e.Seq, Type: e.Event.Type, Attempts: attempts, Outcome: outcome, Cause: cause, StuckAfter: r.policy.StuckAfter})
}
