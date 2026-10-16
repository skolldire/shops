package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/validation"
)

const (
	key      = "6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"
	otherKey = "7f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"
	idA      = "11111111-1111-4111-8111-111111111111"
	idB      = "22222222-2222-4222-8222-222222222222"
)

var budgets = app.Budgets{Reserve: time.Second, Charge: time.Second, Finalize: time.Second}

type fixture struct {
	w        *world
	pay      *provider
	rec      *recorder
	checkout *app.Checkout
	a, b     domain.ID
}

func setup(t *testing.T, m mode) *fixture {
	t.Helper()
	f := &fixture{w: newWorld(), pay: newProvider(m), rec: &recorder{}}
	f.a = f.w.product(idA, "A-1", "29.99", 5)
	f.b = f.w.product(idB, "B-1", "5.00", 10)
	var err error
	f.checkout, err = app.NewCheckout(f.w, f.w, f.pay, f.w, f.w, f.rec, budgets)
	require.NoError(t, err)
	return f
}

func price(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func order(k, token string, items ...domain.CheckoutItemInput) domain.CheckoutInput {
	return domain.CheckoutInput{IdempotencyKey: k, PaymentToken: token, Items: items}
}

func line(id domain.ID, qty int, p string) domain.CheckoutItemInput {
	return domain.CheckoutItemInput{ProductID: string(id), Quantity: qty, ExpectedUnitPrice: price(p)}
}

func (f *fixture) standard() domain.CheckoutInput {
	return order(key, "pm_approved", line(f.a, 2, "29.99"), line(f.b, 1, "5.00"))
}

func (f *fixture) run(t *testing.T, in domain.CheckoutInput) (app.CheckoutResult, error) {
	t.Helper()
	return f.checkout.Checkout(context.Background(), "alice", in)
}

func TestNewKeyApprovedConfirmsOnceAndCleansTheCart(t *testing.T) {
	f := setup(t, approve)
	f.w.state.carts["alice"] = []domain.CartItem{
		{ProductID: f.a, Quantity: 2, UnitPriceSeen: *price("29.99")},
		{ProductID: f.b, Quantity: 3, UnitPriceSeen: *price("5.00")},
	}
	res, err := f.run(t, f.standard())
	require.NoError(t, err)
	require.Equal(t, app.Created, res.Kind)
	p := res.Purchase
	require.Equal(t, domain.StatusConfirmed, p.Status)
	require.Equal(t, "ref_"+string(p.ID), p.PaymentRef)
	require.Equal(t, "64.98", p.Total.StringFixed(2))
	require.Len(t, p.Items, 2)
	require.Equal(t, "59.98", p.Items[0].LineTotal.StringFixed(2))
	require.Equal(t, 3, f.w.stockOf(f.a))
	require.Equal(t, 9, f.w.stockOf(f.b))
	require.Equal(t, 1, f.w.state.commits)
	require.Zero(t, f.w.state.releases)
	require.Zero(t, f.w.state.reserved[f.a], "the reservation becomes a sale")
	require.Equal(t, 1, f.pay.effective())
	require.Equal(t, []domain.CartItem{{ProductID: f.b, Quantity: 3, UnitPriceSeen: *price("5.00")}}, f.w.state.carts["alice"],
		"only the line bought as it was is removed")
	require.Equal(t, []string{app.OutcomeConfirmed}, f.rec.outcomes())
	require.Equal(t, domain.StatusConfirmed, f.rec.events[0].purchase.Status)
	require.Equal(t, app.Charge{Reference: string(p.ID), Token: "pm_approved", Amount: p.Total, Currency: "USD"}, f.pay.requests[0])
}

func TestNewKeyDeclinedReturnsStockOnceAndKeepsTheCart(t *testing.T) {
	f := setup(t, decline)
	cart := []domain.CartItem{{ProductID: f.a, Quantity: 2, UnitPriceSeen: *price("29.99")}}
	f.w.state.carts["alice"] = cart
	res, err := f.run(t, f.standard())
	require.NoError(t, err)
	require.Equal(t, app.Created, res.Kind)
	require.Equal(t, domain.StatusFailed, res.Purchase.Status)
	require.Equal(t, "card_declined", res.Purchase.FailureReason)
	require.Equal(t, 5, f.w.stockOf(f.a))
	require.Equal(t, 10, f.w.stockOf(f.b))
	require.Equal(t, 1, f.w.state.releases)
	require.Zero(t, f.w.state.commits)
	require.Zero(t, f.w.state.reserved[f.a])
	require.Equal(t, cart, f.w.state.carts["alice"])
	require.Equal(t, []string{app.OutcomeDeclined}, f.rec.outcomes())
}

func TestUnknownOutcomeKeepsThePurchasePendingWithTheStockTaken(t *testing.T) {
	for name, m := range map[string]mode{"unknown": unknownAfterCharge, "unavailable after the reservation": unavailable} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, m)
			res, err := f.run(t, f.standard())
			require.NoError(t, err)
			require.Equal(t, app.Created, res.Kind)
			require.Equal(t, domain.StatusPending, res.Purchase.Status)
			require.Equal(t, 3, f.w.stockOf(f.a))
			require.Zero(t, f.w.state.releases)
			require.Equal(t, []string{app.OutcomePending}, f.rec.outcomes())
			require.Error(t, f.rec.events[0].cause)
		})
	}
}

func TestFinalizationFailureAfterAnApprovedChargeKeepsThePurchasePending(t *testing.T) {
	f := setup(t, approve)
	f.w.finErr = errors.New("connection reset")
	res, err := f.run(t, f.standard())
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, res.Purchase.Status)
	require.Equal(t, 1, f.pay.effective())
	require.Equal(t, 3, f.w.stockOf(f.a))
	require.Zero(t, f.w.state.releases)
	require.Equal(t, []string{app.OutcomePending}, f.rec.outcomes())
	require.ErrorContains(t, f.rec.events[0].cause, "connection reset")
}

func TestAReservationThatDoesNotMatchTheStockKeepsThePurchasePending(t *testing.T) {
	f := setup(t, approve)
	f.w.state.carts["alice"] = []domain.CartItem{{ProductID: f.a, Quantity: 2, UnitPriceSeen: *price("29.99")}}
	f.pay.onCharge = func() {
		f.w.mu.Lock()
		defer f.w.mu.Unlock()
		f.w.state.reserved[f.a] = 1
	}
	res, err := f.run(t, f.standard())
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, res.Purchase.Status)
	require.Zero(t, f.w.state.commits)
	require.Equal(t, 1, f.w.state.reserved[f.a], "nothing is corrected silently")
	require.Len(t, f.w.state.carts["alice"], 1, "the cart is not touched when the commit fails")
	require.Equal(t, []string{app.OutcomePending}, f.rec.outcomes())
	require.ErrorContains(t, f.rec.events[0].cause, "fewer than 2 reserved units")
}

func TestSameKeySameHashFinishedReplaysWithoutEffects(t *testing.T) {
	for name, m := range map[string]mode{"confirmed": approve, "failed": decline} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, m)
			first, err := f.run(t, f.standard())
			require.NoError(t, err)
			stock, returns := f.w.stockOf(f.a), f.w.state.releases

			again, err := f.run(t, order(key, "pm_approved", line(f.b, 1, "5"), line(f.a, 2, "29.990")))
			require.NoError(t, err)
			require.Equal(t, app.Replayed, again.Kind)
			require.Equal(t, first.Purchase, again.Purchase)
			require.Equal(t, 1, f.pay.received())
			require.Equal(t, stock, f.w.stockOf(f.a))
			require.Equal(t, returns, f.w.state.releases)
			require.Equal(t, 1, f.w.count())
			require.Equal(t, app.OutcomeReplayed, f.rec.outcomes()[1])
		})
	}
}

func TestSameKeySameHashPendingIsInProgressAndDoesNotChargeAgain(t *testing.T) {
	f := setup(t, unknownBeforeCharge)
	first, err := f.run(t, f.standard())
	require.NoError(t, err)
	require.Equal(t, domain.StatusPending, first.Purchase.Status)
	f.pay.set(approve)

	again, err := f.run(t, f.standard())
	require.NoError(t, err)
	require.Equal(t, app.InProgress, again.Kind)
	require.Equal(t, domain.StatusPending, again.Purchase.Status)
	require.Equal(t, first.Purchase.ID, again.Purchase.ID)
	require.Equal(t, 1, f.pay.received())
	require.Equal(t, 3, f.w.stockOf(f.a))
	require.Equal(t, []string{app.OutcomePending, app.OutcomeInProgress}, f.rec.outcomes())
}

func TestSameKeyOtherHashIsAConflictWithoutChanges(t *testing.T) {
	f := setup(t, approve)
	first, err := f.run(t, f.standard())
	require.NoError(t, err)

	_, err = f.run(t, order(key, "pm_approved", line(f.a, 1, "29.99")))
	require.ErrorIs(t, err, app.ErrIdempotencyConflict)
	stored, err := f.w.Get(context.Background(), "alice", first.Purchase.ID)
	require.NoError(t, err)
	require.Equal(t, first.Purchase, stored)
	require.Equal(t, 3, f.w.stockOf(f.a))
	require.Equal(t, 1, f.pay.received())
	require.Equal(t, app.OutcomeIdempotencyConflict, f.rec.outcomes()[1])
}

func TestTheKeyIsScopedByUser(t *testing.T) {
	f := setup(t, approve)
	_, err := f.run(t, f.standard())
	require.NoError(t, err)
	res, err := f.checkout.Checkout(context.Background(), "bob", f.standard())
	require.NoError(t, err)
	require.Equal(t, app.Created, res.Kind)
	require.Equal(t, 2, f.w.count())
}

func TestRecoverableLockErrorRollsBackAndCountsOnlyTryAgain(t *testing.T) {
	f := setup(t, approve)
	f.w.reserveErr = errors.New("deadlock detected")
	f.w.txErr = app.ErrBusy
	_, err := f.run(t, f.standard())
	require.ErrorIs(t, err, app.ErrBusy)
	require.Zero(t, f.w.count())
	require.Equal(t, 5, f.w.stockOf(f.a))
	require.Zero(t, f.pay.received())
	require.Equal(t, []string{app.OutcomeTryAgain}, f.rec.outcomes())

	f.w.reserveErr, f.w.txErr = nil, nil
	res, err := f.run(t, f.standard())
	require.NoError(t, err)
	require.Equal(t, app.Created, res.Kind, "the client can retry with the same key")
}

func TestUnexpectedFailureCountsOnlyError(t *testing.T) {
	f := setup(t, approve)
	f.w.reserveErr = errors.New("connection refused")
	_, err := f.run(t, f.standard())
	require.ErrorContains(t, err, "connection refused")
	require.NotErrorIs(t, err, app.ErrBusy)
	require.Zero(t, f.w.count())
	require.Equal(t, []string{app.OutcomeError}, f.rec.outcomes())
}

func TestStockRejectionsRollBackEverything(t *testing.T) {
	f := setup(t, approve)
	f.w.product("33333333-3333-4333-8333-333333333333", "C-1", "1.00", 0)
	_, err := f.run(t, order(key, "pm_approved", line(f.a, 1, "29.99"), line("33333333-3333-4333-8333-333333333333", 1, "1.00"), line(f.b, 1, "5.00")))
	var rejected *app.RejectedError
	require.ErrorAs(t, err, &rejected)
	require.ErrorIs(t, err, app.ErrInsufficientStock)
	require.Equal(t, validation.Errors{{Field: "items[1].quantity", Code: app.CodeInsufficientStock, Message: "only 0 available"}}, rejected.Errors)
	require.Zero(t, f.w.count())
	require.Equal(t, 5, f.w.stockOf(f.a))
	require.Equal(t, 10, f.w.stockOf(f.b))

	f.w.state.deleted[f.b] = true
	_, err = f.run(t, order(key, "pm_approved", line(f.a, 1, "29.99"), line(f.b, 1, "5.00")))
	require.ErrorIs(t, err, app.ErrProductUnavailable)
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, "items[1].product_id", rejected.Errors[0].Field)
	require.Equal(t, app.CodeProductUnavailable, rejected.Errors[0].Code)
	require.Zero(t, f.w.count())
	require.Equal(t, []string{app.OutcomeInsufficientStock, app.OutcomeProductUnavailable}, f.rec.outcomes())
}

func TestPriceChangeRollsBackThePurchaseAndTheStock(t *testing.T) {
	f := setup(t, approve)
	_, err := f.run(t, order(key, "pm_approved", line(f.b, 1, "5.00"), line(f.a, 2, "25.00")))
	require.ErrorIs(t, err, app.ErrPriceChanged)
	var rejected *app.RejectedError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, validation.Errors{{Field: "items[1].expected_unit_price", Code: domain.CodePriceChanged, Message: "the current price is 29.99"}}, rejected.Errors)
	require.Zero(t, f.w.count())
	require.Equal(t, 5, f.w.stockOf(f.a))
	require.Equal(t, 10, f.w.stockOf(f.b))
	require.Zero(t, f.pay.received())
	require.Equal(t, []string{app.OutcomePriceChanged}, f.rec.outcomes())
}

func TestInvalidRequestsAreCountedOnce(t *testing.T) {
	f := setup(t, approve)
	_, err := f.run(t, order(key, "", line(f.a, 0, "1")))
	var errs validation.Errors
	require.ErrorAs(t, err, &errs)
	_, err = f.run(t, order("not-a-uuid", "pm_approved", line(f.a, 1, "29.99")))
	require.ErrorIs(t, err, domain.ErrInvalidIdempotencyKey)
	require.Equal(t, []string{app.OutcomeInvalid, app.OutcomeInvalid}, f.rec.outcomes())
	require.Zero(t, f.w.count())
}

func TestCancellingTheRequestAfterTheReservationStillSettles(t *testing.T) {
	f := setup(t, approve)
	ctx, cancel := context.WithCancel(context.Background())
	f.pay.onCharge = cancel
	res, err := f.checkout.Checkout(ctx, "alice", f.standard())
	require.NoError(t, err)
	require.Equal(t, domain.StatusConfirmed, res.Purchase.Status)
	require.Equal(t, []bool{true}, f.pay.seenCtxOK)
}

func TestChargeTimeoutLeavesPendingAndFinalizeUsesAFreshContext(t *testing.T) {
	f := setup(t, blockUntilDone)
	short := app.Budgets{Reserve: time.Second, Charge: 20 * time.Millisecond, Finalize: time.Second}
	c, err := app.NewCheckout(f.w, f.w, f.pay, f.w, f.w, f.rec, short)
	require.NoError(t, err)
	start := time.Now()
	res, err := c.Checkout(context.Background(), "alice", f.standard())
	require.NoError(t, err)
	require.Less(t, time.Since(start), time.Second)
	require.Equal(t, domain.StatusPending, res.Purchase.Status)
	require.ErrorIs(t, f.rec.events[0].cause, context.DeadlineExceeded)

	slow := &slowApproval{delay: 30 * time.Millisecond}
	f2 := setup(t, approve)
	c, err = app.NewCheckout(f2.w, f2.w, slow, f2.w, f2.w, f2.rec, short)
	require.NoError(t, err)
	res, err = c.Checkout(context.Background(), "alice", f2.standard())
	require.NoError(t, err)
	require.Equal(t, domain.StatusConfirmed, res.Purchase.Status, "the finalization does not inherit the expired charge context")
}

type slowApproval struct{ delay time.Duration }

func (s *slowApproval) Lookup(context.Context, string) (app.PaymentRecord, error) {
	return app.PaymentRecord{Status: app.PaymentNotFound}, nil
}

func (s *slowApproval) Void(context.Context, string) (app.PaymentRecord, error) {
	return app.PaymentRecord{Status: app.PaymentVoided}, nil
}

func (s *slowApproval) Charge(_ context.Context, c app.Charge) (app.ChargeResult, error) {
	time.Sleep(s.delay)
	return app.ChargeResult{Approved: true, ProviderRef: "ref_" + c.Reference}, nil
}

func TestConcurrentCheckoutsWithTheSameKeyCreateOnePurchase(t *testing.T) {
	f := setup(t, approve)
	var wg sync.WaitGroup
	kinds := make([]app.ResultKind, 10)
	for i := range kinds {
		wg.Go(func() {
			res, err := f.run(t, f.standard())
			require.NoError(t, err)
			kinds[i] = res.Kind
		})
	}
	wg.Wait()
	require.Equal(t, 1, f.w.count())
	require.Equal(t, 3, f.w.stockOf(f.a))
	require.Equal(t, 1, f.pay.effective())
	created := 0
	for _, k := range kinds {
		if k == app.Created {
			created++
		}
	}
	require.Equal(t, 1, created)
	require.Len(t, f.rec.outcomes(), 10)
}

func TestNewCheckoutValidatesItsDependencies(t *testing.T) {
	w := newWorld()
	_, err := app.NewCheckout(nil, w, newProvider(approve), w, w, &recorder{}, budgets)
	require.Error(t, err)
	_, err = app.NewCheckout(w, w, newProvider(approve), w, w, &recorder{}, app.Budgets{Reserve: time.Second, Charge: 0, Finalize: time.Second})
	require.Error(t, err)
	_, err = app.NewCheckout(w, w, newProvider(approve), w, w, nil, budgets)
	require.Error(t, err)
}

func TestUnknownUserIsAnError(t *testing.T) {
	f := setup(t, approve)
	_, err := f.checkout.Checkout(context.Background(), "", f.standard())
	require.ErrorIs(t, err, domain.ErrUserRequired)
	require.Equal(t, []string{app.OutcomeError}, f.rec.outcomes())
}

func TestOtherKeysCreateOtherPurchases(t *testing.T) {
	f := setup(t, approve)
	_, err := f.run(t, f.standard())
	require.NoError(t, err)
	res, err := f.run(t, order(otherKey, "pm_approved", line(f.a, 1, "29.99")))
	require.NoError(t, err)
	require.Equal(t, app.Created, res.Kind)
	require.Equal(t, 2, f.w.stockOf(f.a))
}
