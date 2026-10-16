package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/validation"
)

func newCart(t *testing.T) (*app.Cart, *fixture) {
	t.Helper()
	f := setup(t, approve)
	c, err := app.NewCart(f.w, f.w)
	require.NoError(t, err)
	return c, f
}

func TestCartAddKeepsThePriceSeenAndMergesQuantities(t *testing.T) {
	c, f := newCart(t)
	ctx := context.Background()
	require.NoError(t, c.Add(ctx, "alice", "11111111-1111-4111-8111-111111111111", 2))
	require.NoError(t, c.Add(ctx, "alice", idA, 98))
	lines, err := f.w.Lines(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, []domain.CartItem{{ProductID: f.a, Quantity: 99, UnitPriceSeen: *price("29.99")}}, lines)

	f.w.state.deleted[f.b] = true
	require.ErrorIs(t, c.Add(ctx, "alice", idB, 1), app.ErrProductUnavailable)
	require.ErrorIs(t, c.Add(ctx, "alice", "33333333-3333-4333-8333-333333333333", 1), app.ErrProductUnavailable)
	require.ErrorIs(t, c.Add(ctx, "alice", "{"+idA+"}", 1), domain.ErrInvalidID)
	var errs validation.Errors
	require.ErrorAs(t, c.Add(ctx, "alice", idA, 0), &errs)
	require.ErrorAs(t, c.SetQuantity(ctx, "alice", idA, 100), &errs)
	require.ErrorIs(t, c.Add(ctx, "", idA, 1), domain.ErrUserRequired)
}

func TestCartViewWarnsAboutChanges(t *testing.T) {
	c, f := newCart(t)
	ctx := context.Background()
	f.w.product("33333333-3333-4333-8333-333333333333", "C-1", "3.00", 1)
	f.w.state.carts["alice"] = []domain.CartItem{
		{ProductID: f.a, Quantity: 2, UnitPriceSeen: *price("25.00")},
		{ProductID: f.b, Quantity: 1, UnitPriceSeen: *price("5.00")},
		{ProductID: "33333333-3333-4333-8333-333333333333", Quantity: 4, UnitPriceSeen: *price("3.00")},
	}
	f.w.state.deleted[f.b] = true

	view, err := c.View(ctx, "alice")
	require.NoError(t, err)
	require.True(t, view.HasWarnings())
	require.True(t, view.Lines[0].PriceChanged)
	require.Equal(t, "29.99", view.Lines[0].Product.Price.StringFixed(2))
	require.False(t, view.Lines[0].LowStock)
	require.True(t, view.Lines[1].Unavailable)
	require.True(t, view.Lines[2].LowStock)
	require.False(t, view.Lines[2].PriceChanged)
	require.Equal(t, "67.00", view.EstimatedTotal.StringFixed(2))
	n, err := c.Count(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, 3, n)

	require.NoError(t, c.AcceptCurrentPrice(ctx, "alice", idA))
	view, err = c.View(ctx, "alice")
	require.NoError(t, err)
	require.False(t, view.Lines[0].PriceChanged)
	require.Equal(t, "29.99", view.Lines[0].Item.UnitPriceSeen.StringFixed(2))
	require.ErrorIs(t, c.AcceptCurrentPrice(ctx, "alice", idB), app.ErrProductUnavailable)

	require.NoError(t, c.SetQuantity(ctx, "alice", idA, 5))
	require.NoError(t, c.Remove(ctx, "alice", "33333333-3333-4333-8333-333333333333"))
	lines, err := f.w.Lines(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, lines, 2)
	require.Equal(t, 5, lines[0].Quantity)
}

func TestCartIsLimitedTo50Lines(t *testing.T) {
	c, f := newCart(t)
	ctx := context.Background()
	for i := range domain.MaxCartLines {
		id := domain.ID("44444444-4444-4444-8444-" + pad(i))
		f.w.product(string(id), "X", "1.00", 1)
		require.NoError(t, c.Add(ctx, "alice", string(id), 1))
	}
	require.ErrorIs(t, c.Add(ctx, "alice", idA, 1), app.ErrCartFull)
	require.NoError(t, c.Add(ctx, "alice", "44444444-4444-4444-8444-"+pad(0), 1), "an existing line does not take a new slot")
}

func pad(i int) string {
	s := "000000000000" + string(rune('0'+i/10)) + string(rune('0'+i%10))
	return s[len(s)-12:]
}

func TestCartCheckoutInputUsesThePricesSeen(t *testing.T) {
	c, f := newCart(t)
	f.w.state.carts["alice"] = []domain.CartItem{{ProductID: f.a, Quantity: 2, UnitPriceSeen: *price("25.00")}}
	in, err := c.CheckoutInput(context.Background(), "alice", key, "pm_declined")
	require.NoError(t, err)
	require.Equal(t, key, in.IdempotencyKey)
	require.Equal(t, "pm_declined", in.PaymentToken)
	require.Equal(t, []domain.CheckoutItemInput{{ProductID: idA, Quantity: 2, ExpectedUnitPrice: price("25.00")}}, in.Items)
}

func TestAFullCartIsAValidCheckoutAndOneMoreLineIsNot(t *testing.T) {
	c, f := newCart(t)
	ctx := context.Background()
	for i := range domain.MaxCartLines {
		id := domain.ID("44444444-4444-4444-8444-" + pad(i))
		f.w.product(string(id), "X", "1.00", 1)
		require.NoError(t, c.Add(ctx, "alice", string(id), 1))
	}
	in, err := c.CheckoutInput(ctx, "alice", key, "pm_approved")
	require.NoError(t, err)
	require.Len(t, in.Items, domain.MaxItems, "the cart holds exactly as many lines as a checkout accepts")
	_, err = domain.NewCheckoutRequest("alice", in)
	require.NoError(t, err, "a full cart can be checked out")

	in.Items = append(in.Items, domain.CheckoutItemInput{ProductID: idA, Quantity: 1, ExpectedUnitPrice: price("29.99")})
	_, err = domain.NewCheckoutRequest("alice", in)
	var errs validation.Errors
	require.ErrorAs(t, err, &errs)
	require.Equal(t, validation.CodeOutOfRange, errs[0].Code)
	require.Equal(t, "items", errs[0].Field)
	require.Contains(t, errs[0].Message, "between 1 and 50 items")
	require.ErrorIs(t, c.Add(ctx, "alice", idA, 1), app.ErrCartFull, "the cart refuses the same extra line")
}
