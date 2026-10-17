package app_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type salesWorld struct {
	inbox map[string]bool
	units map[domain.ID]int
	count map[domain.ID]int
	fail  error
	after func()
}

func newSalesWorld() *salesWorld {
	return &salesWorld{inbox: map[string]bool{}, units: map[domain.ID]int{}, count: map[domain.ID]int{}}
}

func (s *salesWorld) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	inbox, units, count := maps.Clone(s.inbox), maps.Clone(s.units), maps.Clone(s.count)
	err := fn(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		s.inbox, s.units, s.count = inbox, units, count
		return err
	}
	return nil
}

func (s *salesWorld) Record(ctx context.Context, consumer string, eventID domain.ID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	k := consumer + "/" + string(eventID)
	if s.inbox[k] {
		return false, nil
	}
	s.inbox[k] = true
	return true, nil
}

func (s *salesWorld) AddSale(ctx context.Context, productID domain.ID, units int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.fail != nil {
		return s.fail
	}
	s.units[productID] += units
	s.count[productID]++
	if s.after != nil {
		s.after()
	}
	return nil
}

func confirmedEvent(id string) domain.StoredEvent {
	return domain.StoredEvent{EventID: domain.ID(id), Event: domain.PurchaseEvent{Type: domain.EventPurchaseConfirmed, Version: domain.PurchaseEventVersion,
		Items: []domain.EventItem{{ProductID: "p1", Quantity: 2}, {ProductID: "p2", Quantity: 1}}}}
}

func TestTheProjectionAddsAConfirmedPurchaseOnce(t *testing.T) {
	w := newSalesWorld()
	projection, err := app.NewSalesProjection(w, w, w)
	require.NoError(t, err)
	ctx := context.Background()

	applied, err := projection.Handle(ctx, confirmedEvent("e1"))
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = projection.Handle(ctx, confirmedEvent("e1"))
	require.NoError(t, err)
	require.False(t, applied, "a redelivered event is recognised by the inbox")
	require.Equal(t, map[domain.ID]int{"p1": 2, "p2": 1}, w.units)
	require.Equal(t, map[domain.ID]int{"p1": 1, "p2": 1}, w.count, "one confirmed purchase per product")

	failed := domain.StoredEvent{EventID: "e2", Event: domain.PurchaseEvent{Type: domain.EventPurchaseFailed, Version: domain.PurchaseEventVersion, Items: []domain.EventItem{{ProductID: "p1", Quantity: 5}}}}
	applied, err = projection.Handle(ctx, failed)
	require.NoError(t, err)
	require.True(t, applied, "a failed purchase is recorded in the inbox")
	require.Equal(t, 2, w.units["p1"], "and does not change the projection")
}

func TestAProjectionFailureLeavesNoInboxRow(t *testing.T) {
	w := newSalesWorld()
	w.fail = errors.New("check constraint")
	projection, err := app.NewSalesProjection(w, w, w)
	require.NoError(t, err)
	_, err = projection.Handle(context.Background(), confirmedEvent("e1"))
	require.Error(t, err)
	require.Empty(t, w.inbox, "the event is not marked as processed, so it is retried")
	_, err = app.NewSalesProjection(nil, w, w)
	require.Error(t, err)
}

func TestTheProjectionRejectsATypeOrVersionItDoesNotSupport(t *testing.T) {
	w := newSalesWorld()
	projection, err := app.NewSalesProjection(w, w, w)
	require.NoError(t, err)
	newer := confirmedEvent("e1")
	newer.Event.Version = domain.PurchaseEventVersion + 1
	unknown := confirmedEvent("e2")
	unknown.Event.Type = "PurchaseRefunded"
	missing := confirmedEvent("e3")
	missing.Event.Version = 0
	for _, e := range []domain.StoredEvent{newer, unknown, missing} {
		applied, err := projection.Handle(context.Background(), e)
		require.ErrorIs(t, err, domain.ErrUnsupportedEvent, string(e.EventID))
		require.False(t, applied)
	}
	require.Empty(t, w.inbox, "an unsupported event is not recorded as processed")
	require.Empty(t, w.units)
	require.Empty(t, w.count)
}

func TestACancelledProjectionChangesNothing(t *testing.T) {
	w := newSalesWorld()
	projection, err := app.NewSalesProjection(w, w, w)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	applied, err := projection.Handle(ctx, confirmedEvent("e1"))
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, applied)
	require.Empty(t, w.inbox)
	require.Empty(t, w.units)
	require.Empty(t, w.count)
}

func TestACancellationDuringTheProjectionRollsBackEverything(t *testing.T) {
	for name, cancelAfter := range map[string]int{"between two sales": 1, "after the last sale": 2} {
		t.Run(name, func(t *testing.T) {
			w := newSalesWorld()
			projection, err := app.NewSalesProjection(w, w, w)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sales := 0
			w.after = func() {
				sales++
				if sales == cancelAfter {
					cancel()
				}
			}

			applied, err := projection.Handle(ctx, confirmedEvent("e1"))
			require.ErrorIs(t, err, context.Canceled)
			require.False(t, applied)
			require.Equal(t, cancelAfter, sales, "the sales before the cancellation were applied inside the transaction")
			require.Empty(t, w.inbox, "the inbox row is rolled back with them")
			require.Empty(t, w.units)
			require.Empty(t, w.count)

			w.after = nil
			applied, err = projection.Handle(context.Background(), confirmedEvent("e1"))
			require.NoError(t, err)
			require.True(t, applied, "the event is processed when it is delivered again")
			require.Equal(t, map[domain.ID]int{"p1": 2, "p2": 1}, w.units)
		})
	}
}
