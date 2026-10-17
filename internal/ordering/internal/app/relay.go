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

type eventRef struct {
	seq      int64
	kind     domain.EventType
	attempts int
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
	claimed, err := r.queue.Claim(claimCtx, r.policy.Owner, r.policy.Lease, r.policy.Batch)
	cancel()
	if err != nil {
		return RelayReport{}, fmt.Errorf("ordering: claim outbox events: %w", err)
	}
	var report RelayReport
	for _, u := range claimed.Unreadable {
		if ctx.Err() != nil {
			break
		}
		r.setAside(ctx, u, &report)
	}
	for _, e := range claimed.Readable {
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

type relayResult struct {
	outcome   string
	attempts  int
	cause     error
	duplicate bool
}

func (r *Relay) record(ctx context.Context, ref eventRef, res relayResult, report *RelayReport) {
	switch res.outcome {
	case RelayPublished:
		report.Published++
		if res.duplicate {
			report.Duplicates++
			r.rec.RelayDuplicate(ctx)
		}
	case RelaySkipped:
		report.Skipped++
	default:
		report.StillPending++
	}
	r.rec.RelayAttempt(ctx, RelayAttempt{Seq: ref.seq, Type: ref.kind, Attempts: res.attempts, Outcome: res.outcome, Cause: res.cause, StuckAfter: r.policy.StuckAfter})
}

func (r *Relay) renew(ctx context.Context, start time.Time, ref eventRef) (relayResult, bool) {
	renewCtx, cancel := context.WithDeadline(ctx, start.Add(r.policy.DBTimeout))
	owned, err := r.queue.Renew(renewCtx, ref.seq, r.policy.Owner, r.policy.Lease)
	cancel()
	switch {
	case err != nil && ctx.Err() != nil:
		return relayResult{}, false
	case err != nil:
		return relayResult{outcome: RelayError, attempts: ref.attempts, cause: err}, false
	case !owned:
		return relayResult{outcome: RelaySkipped, attempts: ref.attempts}, false
	}
	return relayResult{}, true
}

func (r *Relay) setAside(ctx context.Context, u domain.UnreadableEvent, report *RelayReport) {
	start := time.Now()
	item, cancelItem := context.WithDeadline(context.WithoutCancel(ctx), start.Add(r.item))
	defer cancelItem()
	ref := eventRef{seq: u.Seq, kind: u.Type, attempts: u.Attempts}
	res, owned := r.renew(ctx, start, ref)
	switch {
	case owned:
		r.record(item, ref, r.reschedule(item, ref, u.Cause), report)
	case res.outcome != "":
		r.record(ctx, ref, res, report)
	}
}

func (r *Relay) relay(ctx context.Context, e domain.StoredEvent, report *RelayReport) {
	start := time.Now()
	item, cancelItem := context.WithDeadline(context.WithoutCancel(ctx), start.Add(r.item))
	defer cancelItem()
	ref := eventRef{seq: e.Seq, kind: e.Event.Type, attempts: e.Attempts}
	res, owned := r.renew(ctx, start, ref)
	switch {
	case owned:
		r.record(item, ref, r.deliver(item, e, ref), report)
	case res.outcome != "":
		r.record(ctx, ref, res, report)
	}
}

func (r *Relay) deliver(item context.Context, e domain.StoredEvent, ref eventRef) relayResult {
	applied, err := r.handle(item, e)
	if err != nil {
		return r.reschedule(item, ref, err)
	}
	var owned bool
	markCtx, cancel, err := within(item, r.policy.DBTimeout)
	if err == nil {
		owned, err = r.queue.MarkPublished(markCtx, e.Seq, r.policy.Owner)
		cancel()
	}
	switch {
	case err != nil:
		return relayResult{outcome: RelayError, attempts: ref.attempts, cause: err}
	case !owned:
		return relayResult{outcome: RelaySkipped, attempts: ref.attempts}
	}
	return relayResult{outcome: RelayPublished, attempts: ref.attempts, duplicate: !applied}
}

func (r *Relay) handle(item context.Context, e domain.StoredEvent) (bool, error) {
	ctx, cancel, err := within(item, r.policy.HandlerTimeout)
	if err != nil {
		return false, err
	}
	defer cancel()
	return r.handler.Handle(ctx, e)
}

func (r *Relay) reschedule(item context.Context, ref eventRef, cause error) relayResult {
	var (
		total int
		owned bool
	)
	ctx, cancel, err := within(item, r.policy.DBTimeout)
	if err == nil {
		total, owned, err = r.queue.Reschedule(ctx, ref.seq, r.policy.Owner, r.policy.Backoff(ref.attempts), sanitize(cause))
		cancel()
	}
	switch {
	case err != nil:
		return relayResult{outcome: RelayError, attempts: ref.attempts, cause: errors.Join(cause, err)}
	case !owned:
		return relayResult{outcome: RelaySkipped, attempts: ref.attempts, cause: cause}
	}
	return relayResult{outcome: RelayStillPending, attempts: total, cause: cause}
}
