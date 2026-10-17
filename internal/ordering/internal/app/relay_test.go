package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	markErr        error
	rescheduleErr  error
	kinds          []domain.EventKind
	statsKinds     []domain.EventKind
	occurredAt     map[int64]time.Time
	now            time.Time
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

func (q *fakeQueue) Claim(ctx context.Context, _ string, _ time.Duration, batch int, kinds []domain.EventKind) (app.ClaimedEvents, error) {
	q.note(ctx, "claim", 0, 0)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.kinds = kinds
	if q.claimErr != nil {
		return app.ClaimedEvents{}, q.claimErr
	}
	readable := map[int64]domain.StoredEvent{}
	unreadable := map[int64]domain.UnreadableEvent{}
	var seqs []int64
	for _, e := range q.events {
		if q.pending(e.Seq) && slices.Contains(kinds, domain.EventKind{Type: e.Event.Type, Version: e.Event.Version}) {
			readable[e.Seq] = e
			seqs = append(seqs, e.Seq)
		}
	}
	for _, u := range q.unreadable {
		if q.pending(u.Seq) && slices.Contains(kinds, domain.EventKind{Type: u.Type, Version: u.Version}) {
			unreadable[u.Seq] = u
			seqs = append(seqs, u.Seq)
		}
	}
	slices.Sort(seqs)
	var out app.ClaimedEvents
	for _, seq := range seqs[:min(batch, len(seqs))] {
		if e, ok := readable[seq]; ok {
			out.Readable = append(out.Readable, e)
			continue
		}
		out.Unreadable = append(out.Unreadable, unreadable[seq])
	}
	return out, nil
}

func (q *fakeQueue) pending(seq int64) bool {
	return !slices.Contains(q.published, seq)
}

func (q *fakeQueue) Renew(ctx context.Context, seq int64, _ string, _ time.Duration) (bool, error) {
	q.note(ctx, "renew", seq, 0)
	return !q.lostRenew[seq], q.renewErr
}

func (q *fakeQueue) MarkPublished(ctx context.Context, seq int64, _ string) (bool, error) {
	q.note(ctx, "mark", seq, 0)
	if q.markErr != nil {
		return false, q.markErr
	}
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
	if q.rescheduleErr != nil {
		return 0, false, q.rescheduleErr
	}
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

func (q *fakeQueue) Stats(ctx context.Context, stuckAfter int, kinds []domain.EventKind) (app.EventStats, error) {
	q.note(ctx, "stats", 0, 0)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.statsKinds = kinds
	var (
		stats  app.EventStats
		oldest time.Time
	)
	count := func(seq int64, kind domain.EventKind, attempts int, at time.Time) {
		if !q.pending(seq) {
			return
		}
		stats.Pending++
		if n, ok := q.attempts[seq]; ok {
			attempts = n
		}
		if attempts >= stuckAfter {
			stats.Stuck++
		}
		if !slices.Contains(kinds, kind) {
			stats.Unsupported++
		}
		if !at.IsZero() && (oldest.IsZero() || at.Before(oldest)) {
			oldest = at
		}
	}
	for _, e := range q.events {
		count(e.Seq, domain.EventKind{Type: e.Event.Type, Version: e.Event.Version}, e.Attempts, e.OccurredAt)
	}
	for _, u := range q.unreadable {
		count(u.Seq, domain.EventKind{Type: u.Type, Version: u.Version}, u.Attempts, q.occurredAt[u.Seq])
	}
	if !oldest.IsZero() {
		stats.OldestSeconds = q.now.Sub(oldest).Seconds()
	}
	return stats, nil
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

func (h *fakeHandler) Supports() []domain.EventKind {
	return domain.SupportedEvents()
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
		out[i] = domain.StoredEvent{Seq: s, EventID: domain.ID("event"), Attempts: int(s) - 1, Event: domain.PurchaseEvent{Type: domain.EventPurchaseConfirmed, Version: domain.PurchaseEventVersion}}
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
	require.Equal(t, app.RelayReport{Skipped: 2}, report, "a lost renewal and a lost reschedule both end as skipped")
	require.Equal(t, []string{"claim", "renew", "renew", "reschedule", "stats"}, q.ops(), "a lost renewal does not reschedule")
	require.Empty(t, q.rescheduled, "a lease taken before the reschedule leaves the attempts to the new owner")
	require.Equal(t, []string{app.RelaySkipped, app.RelaySkipped}, rec.outcomes())
}

func TestADuplicateCountsOnlyWhenThisRelayConfirmsItsPublication(t *testing.T) {
	for name, tc := range map[string]struct {
		queue      *fakeQueue
		duplicate  bool
		report     app.RelayReport
		duplicates int
		outcome    string
	}{
		"first delivery confirmed": {&fakeQueue{events: stored(1)}, false, app.RelayReport{Published: 1}, 0, app.RelayPublished},
		"duplicate confirmed":      {&fakeQueue{events: stored(1)}, true, app.RelayReport{Published: 1, Duplicates: 1}, 1, app.RelayPublished},
		"duplicate, lease lost":    {&fakeQueue{events: stored(1), lostMark: map[int64]bool{1: true}}, true, app.RelayReport{Skipped: 1}, 0, app.RelaySkipped},
		"duplicate, confirmation fails": {&fakeQueue{events: stored(1), markErr: errors.New("connection reset")}, true,
			app.RelayReport{StillPending: 1}, 0, app.RelayError},
	} {
		t.Run(name, func(t *testing.T) {
			h, rec := &fakeHandler{duplicate: map[int64]bool{1: tc.duplicate}}, &relayRecorder{}
			report, err := newTestRelay(t, tc.queue, h, rec).RunCycle(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.report, report)
			require.Equal(t, tc.duplicates, rec.duplicates)
			require.Equal(t, []string{tc.outcome}, rec.outcomes())
			require.Empty(t, tc.queue.rescheduled, "an absorbed delivery is not a failure of the consumer")
		})
	}
}

func TestAnotherRelayCountsTheDuplicateWhoseConfirmationFailed(t *testing.T) {
	q, h := &fakeQueue{events: stored(1), markErr: errors.New("connection reset")}, &fakeHandler{duplicate: map[int64]bool{1: true}}
	first := &relayRecorder{}
	report, err := newTestRelay(t, q, h, first).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RelayReport{StillPending: 1}, report)
	require.Zero(t, first.duplicates)

	q.markErr = nil
	second := &relayRecorder{}
	report, err = newTestRelay(t, q, h, second).RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, app.RelayReport{Published: 1, Duplicates: 1}, report, "the relay that confirms the redelivery counts it")
	require.Equal(t, 1, second.duplicates)
	require.Equal(t, []int64{1}, q.published)
}

type limitedHandler struct {
	*fakeHandler
	kinds []domain.EventKind
}

func (h limitedHandler) Supports() []domain.EventKind {
	return h.kinds
}

func TestTheRelayClaimsAndCountsOnlyTheKindsItsConsumerSupports(t *testing.T) {
	kinds := []domain.EventKind{{Type: domain.EventPurchaseConfirmed, Version: domain.PurchaseEventVersion}}
	q := &fakeQueue{events: stored(1)}
	r, err := app.NewRelay(q, limitedHandler{fakeHandler: &fakeHandler{}, kinds: kinds}, &relayRecorder{}, relayPolicy())
	require.NoError(t, err)
	_, err = r.RunCycle(context.Background())
	require.NoError(t, err)
	require.Equal(t, kinds, q.kinds, "the claim is limited to what the consumer supports")
	require.Equal(t, kinds, q.statsKinds, "the unsupported events are counted against the same pairs")
}

const eventRefunded domain.EventType = "PurchaseRefunded"

func pairEvent(seq int64, kind domain.EventType, version int, at time.Time) domain.StoredEvent {
	return domain.StoredEvent{Seq: seq, EventID: domain.ID("event"), OccurredAt: at, Event: domain.PurchaseEvent{Type: kind, Version: version}}
}

func pairUnreadable(seq int64, kind domain.EventType, version int) domain.UnreadableEvent {
	return domain.UnreadableEvent{Seq: seq, EventID: domain.ID("unreadable"), Type: kind, Version: version,
		Cause: fmt.Errorf("%w: an amount that is not a decimal", domain.ErrUnreadablePayload)}
}

func mixedQueue() *fakeQueue {
	return &fakeQueue{
		events: []domain.StoredEvent{
			pairEvent(1, domain.EventPurchaseConfirmed, 1, time.Time{}),
			pairEvent(2, domain.EventPurchaseConfirmed, 2, time.Time{}),
			pairEvent(3, eventRefunded, 1, time.Time{}),
			pairEvent(5, domain.EventPurchaseFailed, 1, time.Time{}),
		},
		unreadable: []domain.UnreadableEvent{
			pairUnreadable(4, domain.EventPurchaseConfirmed, 1),
			pairUnreadable(6, eventRefunded, 1),
		},
	}
}

func seqs(c app.ClaimedEvents) ([]int64, []int64) {
	readable, unreadable := []int64{}, []int64{}
	for _, e := range c.Readable {
		readable = append(readable, e.Seq)
	}
	for _, u := range c.Unreadable {
		unreadable = append(unreadable, u.Seq)
	}
	return readable, unreadable
}

func TestTheFakeQueueClaimsOnlyTheSupportedPairsWithinOneBatch(t *testing.T) {
	supported := []domain.EventKind{{Type: "PurchaseConfirmed", Version: 1}, {Type: "PurchaseFailed", Version: 1}}
	tests := map[string]struct {
		batch          int
		kinds          []domain.EventKind
		published      []int64
		wantReadable   []int64
		wantUnreadable []int64
	}{
		"the batch holds every supported event":           {10, supported, nil, []int64{1, 5}, []int64{4}},
		"the batch is shared by readable and unreadable":  {2, supported, nil, []int64{1}, []int64{4}},
		"a batch of one takes the lowest sequence":        {1, supported, nil, []int64{1}, []int64{}},
		"no supported pair claims nothing":                {10, nil, nil, []int64{}, []int64{}},
		"a published event is not claimed again":          {10, supported, []int64{1}, []int64{5}, []int64{4}},
		"another version of a known type is not claimed":  {10, []domain.EventKind{{Type: "PurchaseConfirmed", Version: 2}}, nil, []int64{2}, []int64{}},
		"an unknown type at a known version is claimable": {10, []domain.EventKind{{Type: "PurchaseRefunded", Version: 1}}, nil, []int64{3}, []int64{6}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			q := mixedQueue()
			q.published = tc.published
			claimed, err := q.Claim(context.Background(), "relay-a", time.Second, tc.batch, tc.kinds)
			require.NoError(t, err)
			readable, unreadable := seqs(claimed)
			require.Equal(t, tc.wantReadable, readable)
			require.Equal(t, tc.wantUnreadable, unreadable)
		})
	}
}

func TestTheFakeQueueStatsCountEveryPendingEvent(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	q := &fakeQueue{
		now: now,
		events: []domain.StoredEvent{
			pairEvent(1, domain.EventPurchaseConfirmed, 1, now.Add(-10*time.Second)),
			pairEvent(2, domain.EventPurchaseConfirmed, 2, now.Add(-30*time.Second)),
			pairEvent(3, eventRefunded, 1, now.Add(-20*time.Second)),
			pairEvent(5, domain.EventPurchaseFailed, 1, now.Add(-90*time.Second)),
		},
		unreadable: []domain.UnreadableEvent{
			pairUnreadable(4, domain.EventPurchaseConfirmed, 1),
			pairUnreadable(6, eventRefunded, 1),
		},
		occurredAt: map[int64]time.Time{4: now.Add(-50 * time.Second), 6: now.Add(-40 * time.Second)},
		attempts:   map[int64]int{2: 5, 4: 3, 5: 9},
		published:  []int64{5},
	}
	stats, err := q.Stats(context.Background(), 3, []domain.EventKind{{Type: "PurchaseConfirmed", Version: 1}, {Type: "PurchaseFailed", Version: 1}})
	require.NoError(t, err)
	require.Equal(t, app.EventStats{Pending: 5, OldestSeconds: 50, Stuck: 2, Unsupported: 3}, stats,
		"pending, oldest and stuck count every unpublished event, unsupported and unreadable ones included; a published event counts in none")
}

func TestTheRelayPublishesOnlyThePairsItsConsumerSupports(t *testing.T) {
	q := mixedQueue()
	h := &fakeHandler{}
	rec := &relayRecorder{}
	r, err := app.NewRelay(q, limitedHandler{fakeHandler: h, kinds: []domain.EventKind{{Type: "PurchaseConfirmed", Version: 1}}}, rec, relayPolicy())
	require.NoError(t, err)

	report, err := r.RunCycle(context.Background())

	require.NoError(t, err)
	require.Equal(t, app.RelayReport{Published: 1, StillPending: 1}, report)
	require.Equal(t, []int64{1}, h.handled, "only the readable event of the supported pair reaches the consumer")
	require.Equal(t, []int64{1}, q.published)
	require.Equal(t, []int64{4}, q.rescheduled, "the unreadable event of the supported pair is set aside")
	require.Len(t, rec.gauges, 1)
	require.Equal(t, 5, rec.gauges[0].Pending, "the published event leaves the pending ones")
	require.Equal(t, 4, rec.gauges[0].Unsupported, "PurchaseConfirmed v2, PurchaseFailed v1 and both PurchaseRefunded v1 events, for this consumer")
}
