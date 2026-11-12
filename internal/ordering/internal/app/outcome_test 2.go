package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

func TestEveryRelayPathRecordsOneOutcomeAndTheMatchingReport(t *testing.T) {
	failing := errors.New("connection reset")
	for name, tc := range map[string]struct {
		queue      *fakeQueue
		handler    *fakeHandler
		outcome    string
		report     app.RelayReport
		duplicates int
	}{
		"published":                   {&fakeQueue{events: stored(1)}, &fakeHandler{}, app.RelayPublished, app.RelayReport{Published: 1}, 0},
		"duplicate published":         {&fakeQueue{events: stored(1)}, &fakeHandler{duplicate: map[int64]bool{1: true}}, app.RelayPublished, app.RelayReport{Published: 1, Duplicates: 1}, 1},
		"renewal lost":                {&fakeQueue{events: stored(1), lostRenew: map[int64]bool{1: true}}, &fakeHandler{}, app.RelaySkipped, app.RelayReport{Skipped: 1}, 0},
		"consumer failure":            {&fakeQueue{events: stored(1)}, &fakeHandler{fail: map[int64]error{1: failing}}, app.RelayStillPending, app.RelayReport{StillPending: 1}, 0},
		"reschedule lost":             {&fakeQueue{events: stored(1), lostReschedule: map[int64]bool{1: true}}, &fakeHandler{fail: map[int64]error{1: failing}}, app.RelaySkipped, app.RelayReport{Skipped: 1}, 0},
		"confirmation lost":           {&fakeQueue{events: stored(1), lostMark: map[int64]bool{1: true}}, &fakeHandler{}, app.RelaySkipped, app.RelayReport{Skipped: 1}, 0},
		"renewal database error":      {&fakeQueue{events: stored(1), renewErr: failing}, &fakeHandler{}, app.RelayError, app.RelayReport{StillPending: 1}, 0},
		"confirmation database error": {&fakeQueue{events: stored(1), markErr: failing}, &fakeHandler{}, app.RelayError, app.RelayReport{StillPending: 1}, 0},
		"reschedule database error":   {&fakeQueue{events: stored(1), rescheduleErr: failing}, &fakeHandler{fail: map[int64]error{1: failing}}, app.RelayError, app.RelayReport{StillPending: 1}, 0},
		"unreadable rescheduled":      {&fakeQueue{unreadable: []domain.UnreadableEvent{unreadable(1, 0)}}, &fakeHandler{}, app.RelayStillPending, app.RelayReport{StillPending: 1}, 0},
		"unreadable reschedule lost":  {&fakeQueue{unreadable: []domain.UnreadableEvent{unreadable(1, 0)}, lostReschedule: map[int64]bool{1: true}}, &fakeHandler{}, app.RelaySkipped, app.RelayReport{Skipped: 1}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &relayRecorder{}
			report, err := newTestRelay(t, tc.queue, tc.handler, rec).RunCycle(context.Background())
			require.NoError(t, err)
			require.Equal(t, []string{tc.outcome}, rec.outcomes(), "one attempt, one outcome")
			require.Equal(t, tc.report, report, "the report follows the approved table for that outcome")
			require.Equal(t, tc.duplicates, rec.duplicates, "a duplicate is a dimension of a confirmed publication")
		})
	}
}

func TestEveryRecoveryPathRecordsOneOutcomeAndTheMatchingReport(t *testing.T) {
	type summary struct{ confirmed, declined, voided, skipped, stillPending int }
	steal := func(f *fixture, op string) {
		f.w.control = func(_ context.Context, called string, id domain.ID) error {
			if called == op {
				f.w.mu.Lock()
				defer f.w.mu.Unlock()
				f.w.state.owners[id] = "worker-b"
			}
			return nil
		}
	}
	fail := func(f *fixture, op string) {
		f.w.control = func(_ context.Context, called string, _ domain.ID) error {
			if called == op {
				return errors.New("connection reset")
			}
			return nil
		}
	}
	for name, tc := range map[string]struct {
		prepare func(t *testing.T, f *fixture)
		outcome string
		want    summary
	}{
		"confirmed": {func(t *testing.T, f *fixture) { f.pending(t); f.pay.set(approve) }, app.RecoveryConfirmed, summary{confirmed: 1}},
		"declined": {func(t *testing.T, f *fixture) {
			f.pay.set(unknownBeforeCharge)
			_, err := f.run(t, f.standard())
			require.NoError(t, err)
			f.pay.set(decline)
		}, app.RecoveryDeclined, summary{declined: 1}},
		"voided": {func(t *testing.T, f *fixture) {
			f.pay.set(unknownBeforeCharge)
			res, err := f.run(t, f.standard())
			require.NoError(t, err)
			f.w.state.attempts[res.Purchase.ID] = stuckAfter
		}, app.RecoveryDeclined, summary{declined: 1, voided: 1}},
		"renewal lost":              {func(t *testing.T, f *fixture) { f.pending(t); steal(f, "renew") }, app.RecoverySkipped, summary{skipped: 1}},
		"provider failure":          {func(t *testing.T, f *fixture) { f.pending(t); f.pay.set(unavailable) }, app.RecoveryStillPending, summary{stillPending: 1}},
		"reschedule lost":           {func(t *testing.T, f *fixture) { f.pending(t); f.pay.set(unavailable); steal(f, "reschedule") }, app.RecoverySkipped, summary{skipped: 1}},
		"renewal database error":    {func(t *testing.T, f *fixture) { f.pending(t); fail(f, "renew") }, app.RecoveryError, summary{stillPending: 1}},
		"reschedule database error": {func(t *testing.T, f *fixture) { f.pending(t); f.pay.set(unavailable); fail(f, "reschedule") }, app.RecoveryError, summary{stillPending: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, unknownAfterCharge)
			tc.prepare(t, f)
			report, err := f.recovery(t).RunCycle(context.Background())
			require.NoError(t, err)
			require.Equal(t, []string{tc.outcome}, f.rec.attemptOutcomes(), "one attempt, one outcome")
			require.Equal(t, tc.want, summary{report.Confirmed, report.Declined, report.Voided, report.Skipped, len(report.StillPending)},
				"the report follows the approved table for that outcome")
		})
	}
}
