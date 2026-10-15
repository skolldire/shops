//go:build integration

package ordering_test

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
	"github.com/skolldire/shops/internal/payment"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
	"github.com/skolldire/shops/internal/platform/validation"
)

const (
	key      = "6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"
	otherKey = "7f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"
)

type mode int

const (
	approve mode = iota
	unknownAfterCharge
	unavailable
)

type provider struct {
	mu       sync.Mutex
	mode     mode
	received int
	attempts []payment.ChargeRequest
	charges  map[string]payment.ChargeRequest
	results  map[string]payment.ChargeResult
	onCharge func(ctx context.Context)
}

func newProvider(m mode) *provider {
	return &provider{mode: m, charges: map[string]payment.ChargeRequest{}, results: map[string]payment.ChargeResult{}}
}

func (p *provider) set(m mode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = m
}

func (p *provider) effective() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.charges)
}

func (p *provider) Charge(ctx context.Context, r payment.ChargeRequest) (payment.ChargeResult, error) {
	if p.onCharge != nil {
		p.onCharge(ctx)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.received++
	p.attempts = append(p.attempts, r)
	if p.mode == unavailable {
		return payment.ChargeResult{}, payment.ErrUnavailable
	}
	if prev, ok := p.charges[r.Reference]; ok {
		if prev.Token != r.Token || !prev.Amount.Equal(r.Amount) || prev.Currency != r.Currency {
			return payment.ChargeResult{}, fmt.Errorf("conflicting content for %s", r.Reference)
		}
		return p.results[r.Reference], nil
	}
	res := payment.ChargeResult{Status: payment.StatusApproved, ProviderRef: "sim_" + r.Reference}
	if r.Token == "pm_declined" {
		res = payment.ChargeResult{Status: payment.StatusDeclined, Reason: payment.ReasonCardDeclined}
	}
	p.charges[r.Reference], p.results[r.Reference] = r, res
	if p.mode == unknownAfterCharge {
		return payment.ChargeResult{}, payment.ErrOutcomeUnknown
	}
	return res, nil
}

type fixture struct {
	pool    *pgxpool.Pool
	catalog *catalog.Module
	orders  *ordering.Module
	pay     *provider
}

func setup(t *testing.T, pay *provider) fixture {
	t.Helper()
	pool, _ := dbtest.Start(t)
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	cat, err := catalog.NewModule(catalog.Deps{Pool: pool, Log: log, Metrics: telemetry.Noop(), Tracer: telemetry.Noop(), Import: catalog.ImportLimits{MaxBytes: 1 << 20, MaxRows: 100}})
	require.NoError(t, err)
	orders, err := ordering.NewModule(ordering.Deps{
		Pool: pool, Catalog: cat, Payments: pay, Log: log, Metrics: telemetry.Noop(), Users: []string{"alice", "bob"},
		Budgets: ordering.Budgets{Reserve: 3 * time.Second, Charge: 3 * time.Second, Finalize: 3 * time.Second, Recovery: 30 * time.Second},
	})
	require.NoError(t, err)
	return fixture{pool: pool, catalog: cat, orders: orders, pay: pay}
}

func (f fixture) product(t *testing.T, sku, price string, stock int64) catalog.Product {
	t.Helper()
	d := decimal.RequireFromString(price)
	w := decimal.RequireFromString("1")
	p, err := f.catalog.Create(context.Background(), catalog.CreateInput{SKU: sku, Name: "Product " + sku, Category: "Tests", Price: &d, Stock: &stock, WeightKg: &w})
	require.NoError(t, err)
	return p
}

func (f fixture) stock(t *testing.T, id string) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT stock FROM products WHERE id = $1::uuid`, id).Scan(&n))
	return n
}

func (f fixture) reserved(t *testing.T, id string) int {
	t.Helper()
	return f.count(t, `SELECT reserved FROM products WHERE id = $1::uuid`, id)
}

func (f fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (f fixture) movements(t *testing.T, reason string) int {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM inventory_movements WHERE reason = $1`, reason)
}

func (f fixture) cartQuantities(t *testing.T, user string) map[string]int {
	t.Helper()
	cart, err := f.orders.Cart(context.Background(), user)
	require.NoError(t, err)
	out := map[string]int{}
	for _, l := range cart.Lines {
		out[l.ProductID] = l.Quantity
	}
	return out
}

func TestCheckoutFromTheCartConfirmsAndCleansTheCart(t *testing.T) {
	f := setup(t, newProvider(approve))
	ctx := context.Background()
	a, b := f.product(t, "A-1", "29.99", 5), f.product(t, "B-1", "5.00", 3)
	require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 2))
	require.NoError(t, f.orders.AddToCart(ctx, "alice", b.ID, 1))

	res, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.NoError(t, err)
	require.Equal(t, ordering.Created, res.Kind)
	require.Equal(t, ordering.StatusConfirmed, res.Purchase.Status)
	require.Equal(t, "64.98", res.Purchase.Total.StringFixed(2))
	require.Equal(t, "sim_"+res.Purchase.ID, res.Purchase.PaymentRef)
	require.Equal(t, 3, f.stock(t, a.ID))
	require.Equal(t, 2, f.stock(t, b.ID))
	require.Empty(t, f.cartQuantities(t, "alice"))

	stored, err := f.orders.Purchase(ctx, "alice", res.Purchase.ID)
	require.NoError(t, err)
	require.Equal(t, res.Purchase, stored, "the stored purchase is the one returned")

	_, err = f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	var errs validation.Errors
	require.ErrorAs(t, err, &errs, "an empty cart cannot be checked out")
	require.Equal(t, "items", errs[0].Field)
}

func TestRollbackInOrderingUndoesTheCatalogDecrement(t *testing.T) {
	f := setup(t, newProvider(approve))
	ctx := context.Background()
	a, b, c := f.product(t, "A-1", "1.00", 5), f.product(t, "B-1", "1.00", 5), f.product(t, "C-1", "1.00", 1)
	for _, p := range []catalog.Product{a, b} {
		require.NoError(t, f.orders.AddToCart(ctx, "alice", p.ID, 2))
	}
	require.NoError(t, f.orders.AddToCart(ctx, "alice", c.ID, 1))
	require.NoError(t, f.orders.SetQuantity(ctx, "alice", c.ID, 2))

	_, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.ErrorIs(t, err, ordering.ErrInsufficientStock)
	var rejected *ordering.RejectedError
	require.ErrorAs(t, err, &rejected)
	require.Contains(t, rejected.Errors[0].Message, "only 1 available")
	require.Equal(t, []int{5, 5, 1}, []int{f.stock(t, a.ID), f.stock(t, b.ID), f.stock(t, c.ID)})
	require.Zero(t, f.count(t, `SELECT count(*) FROM purchases`))
	require.Zero(t, f.count(t, `SELECT count(*) FROM purchase_items`))
	require.Zero(t, f.movements(t, "sale"))
}

func TestPriceChangeRollsBackPurchaseStockAndMovements(t *testing.T) {
	f := setup(t, newProvider(approve))
	ctx := context.Background()
	a := f.product(t, "A-1", "10.00", 5)
	require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 1))
	newPrice := decimal.RequireFromString("12.50")
	_, err := f.catalog.Update(ctx, a.ID, catalog.UpdateInput{Version: a.Version, Price: &newPrice})
	require.NoError(t, err)

	_, err = f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.ErrorIs(t, err, ordering.ErrPriceChanged)
	var rejected *ordering.RejectedError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, "items[0].expected_unit_price", rejected.Errors[0].Field)
	require.Contains(t, rejected.Errors[0].Message, "12.50")
	require.Equal(t, 5, f.stock(t, a.ID))
	require.Zero(t, f.count(t, `SELECT count(*) FROM purchases`))
	require.Zero(t, f.movements(t, "sale"))

	cart, err := f.orders.Cart(ctx, "alice")
	require.NoError(t, err)
	require.True(t, cart.Lines[0].PriceChanged)
	require.NoError(t, f.orders.AcceptCurrentPrice(ctx, "alice", a.ID))
	res, err := f.orders.CheckoutCart(ctx, "alice", otherKey, "pm_approved")
	require.NoError(t, err)
	require.Equal(t, ordering.StatusConfirmed, res.Purchase.Status)
	require.Equal(t, "12.50", res.Purchase.Total.StringFixed(2))
}

func TestDeclinedCheckoutReturnsStockOnceAndKeepsTheCart(t *testing.T) {
	f := setup(t, newProvider(approve))
	ctx := context.Background()
	a := f.product(t, "A-1", "10.00", 5)
	require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 2))
	res, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_declined")
	require.NoError(t, err)
	require.Equal(t, ordering.StatusFailed, res.Purchase.Status)
	require.Equal(t, "card_declined", res.Purchase.FailureReason)
	require.Equal(t, 5, f.stock(t, a.ID))
	require.Zero(t, f.reserved(t, a.ID))
	require.Equal(t, 1, f.movements(t, "reservation_release"))
	require.Zero(t, f.movements(t, "sale"))
	require.Equal(t, map[string]int{a.ID: 2}, f.cartQuantities(t, "alice"))
}

func TestConcurrentFinalizationsApplyTheirEffectsOnce(t *testing.T) {
	for name, token := range map[string]string{"declined": "pm_declined", "confirmed": "pm_approved"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, newProvider(unknownAfterCharge))
			ctx := context.Background()
			a := f.product(t, "A-1", "10.00", 5)
			require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 2))
			res, err := f.orders.CheckoutCart(ctx, "alice", key, token)
			require.NoError(t, err)
			require.Equal(t, ordering.StatusPending, res.Purchase.Status)
			f.pay.set(approve)

			var wg sync.WaitGroup
			reports := make([]ordering.RecoveryReport, 4)
			for i := range reports {
				wg.Go(func() {
					report, err := f.orders.RecoverPending(ctx)
					require.NoError(t, err)
					reports[i] = report
				})
			}
			wg.Wait()

			if token == "pm_declined" {
				require.Equal(t, 5, f.stock(t, a.ID))
				require.Equal(t, 1, f.movements(t, "reservation_release"))
				require.Zero(t, f.movements(t, "sale"))
			} else {
				require.Equal(t, 3, f.stock(t, a.ID))
				require.Zero(t, f.movements(t, "reservation_release"))
				require.Equal(t, 1, f.movements(t, "sale"))
				require.Empty(t, f.cartQuantities(t, "alice"))
			}
			require.Zero(t, f.reserved(t, a.ID))
			require.Equal(t, 1, f.movements(t, "reservation"))
			require.Equal(t, 1, f.pay.effective())
			p, err := f.orders.Purchase(ctx, "alice", res.Purchase.ID)
			require.NoError(t, err)
			require.NotEqual(t, ordering.StatusPending, p.Status)
		})
	}
}

func TestACartLineChangedDuringThePaymentIsKept(t *testing.T) {
	pay := newProvider(approve)
	f := setup(t, pay)
	ctx := context.Background()
	a, b := f.product(t, "A-1", "10.00", 9), f.product(t, "B-1", "4.00", 9)
	require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 2))
	require.NoError(t, f.orders.AddToCart(ctx, "alice", b.ID, 1))
	pay.onCharge = func(ctx context.Context) {
		require.NoError(t, f.orders.SetQuantity(ctx, "alice", a.ID, 5))
	}

	res, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.NoError(t, err)
	require.Equal(t, ordering.StatusConfirmed, res.Purchase.Status)
	require.Equal(t, map[string]int{a.ID: 5}, f.cartQuantities(t, "alice"), "the changed line stays as it was left, the other one is removed")
}

func TestCartChangesConcurrentWithFinalizationDoNotDeadlock(t *testing.T) {
	f := setup(t, newProvider(approve))
	ctx := context.Background()
	a, b := f.product(t, "A-1", "10.00", 50), f.product(t, "B-1", "4.00", 50)
	require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 1))
	require.NoError(t, f.orders.AddToCart(ctx, "alice", b.ID, 1))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for q := 2; ; q = q%9 + 2 {
			select {
			case <-stop:
				return
			default:
			}
			require.NoError(t, f.orders.SetQuantity(ctx, "alice", b.ID, q))
			require.NoError(t, f.orders.AcceptCurrentPrice(ctx, "alice", b.ID))
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("checkout blocked")
	}
	close(stop)
	wg.Wait()
	require.NotContains(t, f.cartQuantities(t, "alice"), a.ID)
}

func TestTheCartAcceptsAtMost50LinesUnderConcurrency(t *testing.T) {
	f := setup(t, newProvider(approve))
	ctx := context.Background()
	for i := range 49 {
		p := f.product(t, fmt.Sprintf("L-%02d", i), "1.00", 1)
		require.NoError(t, f.orders.AddToCart(ctx, "alice", p.ID, 1))
	}
	x, y := f.product(t, "X-1", "1.00", 1), f.product(t, "Y-1", "1.00", 1)

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, p := range []catalog.Product{x, y} {
		wg.Go(func() { errs[i] = f.orders.AddToCart(ctx, "alice", p.ID, 1) })
	}
	wg.Wait()
	require.Equal(t, 50, f.count(t, `SELECT count(*) FROM cart_items WHERE user_id = 'alice'`))
	full := 0
	for _, err := range errs {
		if err != nil {
			require.ErrorIs(t, err, ordering.ErrCartFull)
			full++
		}
	}
	require.Equal(t, 1, full)
	require.NoError(t, f.orders.AddToCart(ctx, "bob", x.ID, 1), "another user is not affected")
}

func TestRecoveryWithAnUnavailableProviderKeepsPendingAndConfirmsLater(t *testing.T) {
	f := setup(t, newProvider(unknownAfterCharge))
	ctx := context.Background()
	a := f.product(t, "A-1", "10.00", 5)
	require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 1))
	res, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.NoError(t, err)
	require.Equal(t, ordering.StatusPending, res.Purchase.Status)
	require.Equal(t, []int{5, 1}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)})

	f.pay.set(unavailable)
	report, err := f.orders.RecoverPending(ctx)
	require.NoError(t, err)
	require.Len(t, report.StillPending, 1)
	require.Equal(t, res.Purchase.ID, report.StillPending[0].ID)
	require.Equal(t, []int{5, 1}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)})

	f.pay.set(approve)
	report, err = f.orders.RecoverPending(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, report.Confirmed)
	require.Equal(t, 1, f.pay.effective())
	p, err := f.orders.Purchase(ctx, "alice", res.Purchase.ID)
	require.NoError(t, err)
	require.Equal(t, ordering.StatusConfirmed, p.Status)
	require.Equal(t, "sim_"+p.ID, p.PaymentRef)
	require.Equal(t, []int{4, 0}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)})

	report, err = f.orders.RecoverPending(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RecoveryReport{}, report)
}

func TestFacadeRejectsInvalidUUIDs(t *testing.T) {
	f := setup(t, newProvider(approve))
	ctx := context.Background()
	a := f.product(t, "A-1", "10.00", 5)
	for _, raw := range []string{"urn:uuid:" + a.ID, "{" + a.ID + "}", "nope"} {
		require.ErrorIs(t, f.orders.AddToCart(ctx, "alice", raw, 1), ordering.ErrInvalidID)
		_, err := f.orders.Purchase(ctx, "alice", raw)
		require.ErrorIs(t, err, ordering.ErrPurchaseNotFound)
	}
	_, err := f.orders.CheckoutCart(ctx, "alice", "{"+key+"}", "pm_approved")
	require.ErrorIs(t, err, ordering.ErrInvalidIdempotencyKey)
}

func TestPurchasesArePagedNewestFirstAndScopedToTheUser(t *testing.T) {
	f := setup(t, newProvider(approve))
	ctx := context.Background()
	a := f.product(t, "A-1", "1.00", 10)
	var ids []string
	for _, k := range []string{key, otherKey} {
		require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 1))
		res, err := f.orders.CheckoutCart(ctx, "alice", k, "pm_approved")
		require.NoError(t, err)
		ids = append(ids, res.Purchase.ID)
	}
	page, err := f.orders.Purchases(ctx, "alice", 1, 1)
	require.NoError(t, err)
	require.Equal(t, 2, page.Total)
	require.Equal(t, ids[1], page.Items[0].ID)
	page, err = f.orders.Purchases(ctx, "alice", 2, 1)
	require.NoError(t, err)
	require.Equal(t, ids[0], page.Items[0].ID)
	require.Len(t, page.Items[0].Items, 1)

	page, err = f.orders.Purchases(ctx, "bob", 0, 0)
	require.NoError(t, err)
	require.Zero(t, page.Total)
	_, err = f.orders.Purchase(ctx, "bob", ids[0])
	require.ErrorIs(t, err, ordering.ErrPurchaseNotFound)
}

func TestAC07UnknownOutcomeIsRecoveredWithoutASecondCharge(t *testing.T) {
	f := setup(t, newProvider(unknownAfterCharge))
	ctx := context.Background()
	a := f.product(t, "A-1", "10.00", 5)
	require.NoError(t, f.orders.AddToCart(ctx, "alice", a.ID, 2))
	res, err := f.orders.CheckoutCart(ctx, "alice", key, "pm_approved")
	require.NoError(t, err)
	require.Equal(t, ordering.StatusPending, res.Purchase.Status)
	require.Equal(t, []int{5, 2}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)}, "the units stay reserved while the outcome is unknown")
	require.Equal(t, 1, f.pay.effective())
	charged := f.pay.charges[res.Purchase.ID]

	f.pay.set(approve)
	report, err := f.orders.RecoverPending(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RecoveryReport{Confirmed: 1}, report)
	require.Equal(t, 1, f.pay.effective(), "no second charge")
	require.Equal(t, 2, f.pay.received)
	require.Equal(t, charged, f.pay.charges[res.Purchase.ID], "same reference, token, amount and currency")
	p, err := f.orders.Purchase(ctx, "alice", res.Purchase.ID)
	require.NoError(t, err)
	require.Equal(t, ordering.StatusConfirmed, p.Status)
	require.Equal(t, []int{3, 0}, []int{f.stock(t, a.ID), f.reserved(t, a.ID)})
	require.Empty(t, f.cartQuantities(t, "alice"))

	report, err = f.orders.RecoverPending(ctx)
	require.NoError(t, err)
	require.Equal(t, ordering.RecoveryReport{}, report, "running it again changes nothing")
	require.Equal(t, 2, f.pay.received)
	require.Equal(t, 1, f.movements(t, "sale"))
}
