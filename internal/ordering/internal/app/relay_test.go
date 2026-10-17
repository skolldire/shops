package app_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type queueCall struct {
	op       string
	seq      int64
	deadline time.Time
	backoff  time.Duration
}

type fakeQueue struct {
	mu             sync.Mutex
	events         []domain.StoredEvent
	unreadable     []domain.UnreadableEvent
	calls          []queueCall
	lostRenew      map[int64]bool
	lostMark       map[int64]bool
	lostReschedule map[int64]bool
	attempts       map[int64]int
	causes         map[int64]string
	renewErr       error
	claimErr       error
	published      []int64
	rescheduled    []int64
}

func (q *fakeQueue) note(ctx context.Context, op string, seq int64, backoff time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	deadline, _ := ctx.Deadline()
	q.calls = append(q.calls, queueCall{op: op, seq: seq, deadline: deadline, backoff: backoff})
}

func (q *fakeQueue) Claim(ctx context.Context, _ string, _ time.Duration, batch int) (app.ClaimedEvents, error) {
	q.note(ctx, "claim", 0, 0)
	if q.claimErr != nil {
		return app.ClaimedEvents{}, q.claimErr
	}
	return app.ClaimedEvents{Readable: q.events[:min(batch, len(q.events))], Unreadable: q.unreadable}, nil
}

func (q *fakeQueue) Renew(ctx context.Context, seq int64, _ string, _ time.Duration) (bool, error) {
	q.note(ctx, "renew", seq, 0)
	return !q.lostRenew[seq], q.renewErr
}

func (q *fakeQueue) MarkPublished(ctx context.Context, seq int64, _ string) (bool, error) {
	q.note(ctx, "mark", seq, 0)
	if q.lostMark[seq] {
		return false, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.published = append(q.published, seq)
	return true, nil
}

func (q *fakeQueue) Reschedule(ctx context.Context, seq int64, _ string, backoff time.Duration, cause string) (int, bool, error) {
	q.note(ctx, "reschedule", seq, backoff)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.lostReschedule[seq] {
		return 0, false, nil
	}
	q.rescheduled = append(q.rescheduled, seq)
	if q.attempts == nil {
		q.attempts = map[int64]int{}
	}
	if q.causes == nil {
		q.causes = map[int64]string{}
	}
	q.attempts[seq]++
	q.causes[seq] = cause
	return q.attempts[seq], true, nil
}

func (q *fakeQueue) Stats(ctx context.Context, _ int) (app.EventStats, error) {
	q.note(ctx, "stats", 0, 0)
	return app.EventStats{Pending: len(q.events)}, nil
}

func (q *fakeQueue) ops() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, len(q.calls))
	for i, c := range q.calls {
		out[i] = c.op
	}
	return out
}

type fakeHandler struct {
	mu        sync.Mutex
	handled   []int64
	deadlines []time.Time
	fail      map[int64]error
	duplicate map[int64]bool
	onHandle  func()
}

func (h *fakeHandler) Handle(ctx context.Context, e domain.StoredEvent) (bool, error) {
	h.mu.Lock()
	h.handled = append(h.handled, e.Seq)
	deadline, _ := ctx.Deadline()
	h.deadlines = append(h.deadlines, deadline)
	hook := h.onHandle
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err := h.fail[e.Seq]; err != nil {
		return false, err
	}
	return !h.duplicate[e.Seq], nil
}

type relayRecorder struct {
	mu         sync.Mutex
	attempts   []app.RelayAttempt
	duplicates int
	gauges     []app.EventStats
}

func (r *relayRecorder) RelayAttempt(_ context.Context, a app.RelayAttempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts = append(r.attempts, a)
}

func (r *relayRecorder) RelayDuplicate(context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.duplicates++
}

func (r *relayRecorder) RelayGauges(_ context.Context, s app.EventStats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges = append(r.gauges, s)
}

func (r *relayRecorder) outcomes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.attempts))
	for i, a := range r.attempts {
		out[i] = a.Outcome
	}
	return out
}

func relayPolicy() app.RelayPolicy {
	return app.RelayPolicy{
		Owner: "relay-a", Lease: 10 * time.Second, DBTimeout: time.Second, HandlerTimeout: 2 * time.Second, Batch: 10, StuckAfter: 3,
		Backoff: func(attempts int) time.Duration { return time.Duration(attempts+1) * time.Second },
	}
}

func stored(seqs ...int64) []domain.StoredEvent {
	out := make([]domain.StoredEvent, len(seqs))
	for i, s := range seqs {
		out[i] = domain.StoredEvent{Seq: s, EventID: domain.ID("event"), Attempts: int(s) - 1, Event: domain.PurchaseEvent{Type: domain.EventPurchaseConfirmed}}
	}
	return out
}

func unreadable(seq int64, attempts int) domain.UnreadableEvent {
	return domain.UnreadableEvent{Seq: seq, EventID: domain.ID("unreadable"), Type: domain.EventPurchaseConfirmed, Version: domain.PurchaseEventVersion,
		Attempts: attempts, Cause: fmt.Errorf("%w: an amount that is not a decimal", domain.ErrUnreadablePayload)}
}

func newTestRelay(t *testing.T, q *fakeQueue, h *fakeHandler, rec *relayRecorder) *app.Relay {
	t.Helper()
	r, err := app.NewRelay(q, h, rec, relayPolicy())
	require.NoError(t, err)
	return r
}

func TestTheRelayPublishesEachClaimedEventWithinItsDeadlines(t *testing.T) {
	q, h, rec := &fakeQueue{events: stored(1, 2)}, &fakeHandler{duplicate: map[int64]bool{2: true}}, &relayRecorder{}
	started := time.Now()

	report, err := newTestRelay(t, q, h, rec).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RelayReport{Published: 2, Duplicates: 1}, report)
	require.Equal(t, []string{"claim", "renew", "mark", "renew", "mark", "stats"}, q.ops())
	require.Equal(t, []int64{1, 2}, q.published)
	require.Equal(t, 1, rec.duplicates, "a redelivered event is counted as a duplicate and still published")
	require.Equal(t, []string{app.RelayPublished, app.RelayPublished}, rec.outcomes())
	require.Len(t, rec.gauges, 1)
	item := app.EventItemBudget(time.Second, 2*time.Second)
	for _, c := range q.calls {
		require.False(t, c.deadline.IsZero(), c.op)
		require.False(t, c.deadline.After(time.Now().Add(time.Second)), "%s is bounded by the database timeout", c.op)
	}
	for _, d := range h.deadlines {
		require.False(t, d.After(started.Add(item)), "the handler ends within the event deadline")
	}
}

func TestALostRenewalSkipsTheEventWithoutHandlingIt(t *testing.T) {
	q, h, rec := &fakeQueue{events: stored(1, 2), lostRenew: map[int64]bool{1: true}}, &fakeHandler{}, &relayRecorder{}
	report, err := newTestRelay(t, q, h, rec).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RelayReport{Published: 1, Skipped: 1}, report)
	require.Equal(t, []int64{2}, h.handled, "another replica took the first event")
}

func TestAFailingHandlerReschedulesWithBackoffAndCountsTheAttempt(t *testing.T) {
	q, h, rec := &fakeQueue{events: stored(3)}, &fakeHandler{fail: map[int64]error{3: errors.New("projection unavailable")}}, &relayRecorder{}
	report, err := newTestRelay(t, q, h, rec).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RelayReport{StillPending: 1}, report)
	require.Equal(t, []int64{3}, q.rescheduled)
	require.Equal(t, 3*time.Second, q.calls[2].backoff, "the backoff grows with the attempts the event already has")
	require.Empty(t, q.published)
	require.Len(t, rec.attempts, 1)
	require.Equal(t, app.RelayAttempt{Seq: 3, Type: domain.EventPurchaseConfirmed, Attempts: 1, Outcome: app.RelayStillPending,
		Cause: rec.attempts[0].Cause, StuckAfter: 3}, rec.attempts[0])
	require.ErrorContains(t, rec.attempts[0].Cause, "projection unavailable")
}

func TestALostConfirmationIsCountedAsSkipped(t *testing.T) {
	q, h, rec := &fakeQueue{events: stored(1), lostMark: map[int64]bool{1: true}}, &fakeHandler{}, &relayRecorder{}
	report, err := newTestRelay(t, q, h, rec).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RelayReport{Skipped: 1}, report, "another replica published it after this lease expired")
	require.Equal(t, []string{app.RelaySkipped}, rec.outcomes())
}

func TestStoppingTheRelayStartsNoOtherEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q, rec := &fakeQueue{events: stored(1, 2)}, &relayRecorder{}
	h := &fakeHandler{onHandle: cancel}
	report, err := newTestRelay(t, q, h, rec).RunCycle(ctx)
	require.NoError(t, err)
	require.Equal(t, app.RelayReport{Published: 1}, report, "the event in progress finishes")
	require.Equal(t, []string{"claim", "renew", "mark"}, q.ops(), "neither the second event nor the gauges are read after stopping")
}

func TestAClaimFailureEndsTheCycle(t *testing.T) {
	q := &fakeQueue{claimErr: errors.New("connection refused")}
	_, err := newTestRelay(t, q, &fakeHandler{}, &relayRecorder{}).RunCycle(context.Background())
	require.ErrorContains(t, err, "connection refused")
}

func TestNewRelayValidatesItsPolicy(t *testing.T) {
	require.Equal(t, 4*time.Second, app.EventItemBudget(time.Second, 2*time.Second))
	_, err := app.NewRelay(nil, &fakeHandler{}, &relayRecorder{}, relayPolicy())
	require.Error(t, err)
	for name, change := range map[string]func(p *app.RelayPolicy){
		"owner":   func(p *app.RelayPolicy) { p.Owner = "" },
		"backoff": func(p *app.RelayPolicy) { p.Backoff = nil },
		"lease":   func(p *app.RelayPolicy) { p.Lease = 6 * time.Second },
		"db":      func(p *app.RelayPolicy) { p.DBTimeout = 0 },
		"handler": func(p *app.RelayPolicy) { p.HandlerTimeout = 0 },
		"batch":   func(p *app.RelayPolicy) { p.Batch = 0 },
		"stuck":   func(p *app.RelayPolicy) { p.StuckAfter = 0 },
	} {
		p := relayPolicy()
		change(&p)
		_, err := app.NewRelay(&fakeQueue{}, &fakeHandler{}, &relayRecorder{}, p)
		require.Error(t, err, name)
	}
}

func TestAnUnreadableEventIsRescheduledAndTheRestOfTheBatchIsPublished(t *testing.T) {
	q := &fakeQueue{events: stored(2, 3), unreadable: []domain.UnreadableEvent{unreadable(1, 0)}}
	h, rec := &fakeHandler{}, &relayRecorder{}

	report, err := newTestRelay(t, q, h, rec).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RelayReport{Published: 2, StillPending: 1}, report)
	require.Equal(t, []string{"claim", "renew", "reschedule", "renew", "mark", "renew", "mark", "stats"}, q.ops())
	require.Equal(t, []int64{2, 3}, h.handled, "the unreadable event never reaches the consumer")
	require.Equal(t, []int64{2, 3}, q.published)
	require.Equal(t, []int64{1}, q.rescheduled)
	require.Equal(t, time.Second, q.calls[2].backoff, "the same backoff as a failed delivery")
	require.Equal(t, "ordering: the event payload does not match its type and version: an amount that is not a decimal", q.causes[1])
	require.Equal(t, app.RelayStillPending, rec.attempts[0].Outcome)
	require.Equal(t, 1, rec.attempts[0].Attempts)
	require.ErrorIs(t, rec.attempts[0].Cause, domain.ErrUnreadablePayload)
	require.Len(t, rec.gauges, 1, "the gauges are updated after a batch with an unreadable event")
}

func TestAnUnreadableEventReachesTheStuckThresholdLikeAFailedDelivery(t *testing.T) {
	q := &fakeQueue{unreadable: []domain.UnreadableEvent{unreadable(1, 2)}, attempts: map[int64]int{1: 2}}
	rec := &relayRecorder{}
	_, err := newTestRelay(t, q, &fakeHandler{}, rec).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RelayAttempt{Seq: 1, Type: domain.EventPurchaseConfirmed, Attempts: 3, Outcome: app.RelayStillPending,
		Cause: rec.attempts[0].Cause, StuckAfter: 3}, rec.attempts[0], "the recorder logs the one error at the threshold")
	require.Equal(t, 3*time.Second, q.calls[2].backoff)
}

func TestAnUnreadableEventThatAnotherRelayOwnsIsLeftAlone(t *testing.T) {
	q := &fakeQueue{unreadable: []domain.UnreadableEvent{unreadable(1, 0), unreadable(2, 0)},
		lostRenew: map[int64]bool{1: true}, lostReschedule: map[int64]bool{2: true}}
	rec := &relayRecorder{}
	report, err := newTestRelay(t, q, &fakeHandler{}, rec).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RelayReport{Skipped: 1, StillPending: 1}, report)
	require.Equal(t, []string{"claim", "renew", "renew", "reschedule", "stats"}, q.ops(), "a lost renewal does not reschedule")
	require.Empty(t, q.rescheduled, "a lease taken before the reschedule leaves the attempts to the new owner")
	require.Equal(t, []string{app.RelaySkipped, app.RelaySkipped}, rec.outcomes())
}
