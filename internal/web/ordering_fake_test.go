package web

import (
	"context"
	"errors"
	"slices"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/ordering"
)

const testOrigin = "http://example.com"

var testUsers = []string{"alice", "bob", "carol"}

type cartCall struct {
	op, user, product string
	quantity          int
}

type fakeOrdering struct {
	countErr  error
	counted   []string
	cart      ordering.Cart
	cartErr   error
	calls     []cartCall
	writeErr  error
	result    ordering.CheckoutResult
	checkout  error
	keys      []string
	tokens    []string
	purchases map[string]ordering.Purchase
	listed    []int
}

func (f *fakeOrdering) CartCount(_ context.Context, user string) (int, error) {
	f.counted = append(f.counted, user)
	return len(f.cart.Lines), f.countErr
}

func (f *fakeOrdering) Cart(_ context.Context, user string) (ordering.Cart, error) {
	f.calls = append(f.calls, cartCall{op: "view", user: user})
	return f.cart, f.cartErr
}

func (f *fakeOrdering) record(op, user, product string, quantity int) error {
	f.calls = append(f.calls, cartCall{op: op, user: user, product: product, quantity: quantity})
	return f.writeErr
}

func (f *fakeOrdering) AddToCart(_ context.Context, user, product string, quantity int) error {
	return f.record("add", user, product, quantity)
}

func (f *fakeOrdering) SetQuantity(_ context.Context, user, product string, quantity int) error {
	return f.record("quantity", user, product, quantity)
}

func (f *fakeOrdering) RemoveFromCart(_ context.Context, user, product string) error {
	return f.record("remove", user, product, 0)
}

func (f *fakeOrdering) AcceptCurrentPrice(_ context.Context, user, product string) error {
	return f.record("accept", user, product, 0)
}

func (f *fakeOrdering) CheckoutCart(_ context.Context, user, key, token string) (ordering.CheckoutResult, error) {
	f.calls = append(f.calls, cartCall{op: "checkout", user: user})
	f.keys, f.tokens = append(f.keys, key), append(f.tokens, token)
	return f.result, f.checkout
}

func (f *fakeOrdering) Purchase(_ context.Context, user, id string) (ordering.Purchase, error) {
	p, ok := f.purchases[user+"/"+id]
	if !ok {
		return ordering.Purchase{}, ordering.ErrPurchaseNotFound
	}
	return p, nil
}

func (f *fakeOrdering) Purchases(_ context.Context, user string, page, pageSize int) (ordering.PurchasePage, error) {
	f.listed = append(f.listed, page)
	var items []ordering.Purchase
	for k, p := range f.purchases {
		if len(k) > len(user) && k[:len(user)+1] == user+"/" {
			items = append(items, p)
		}
	}
	slices.SortFunc(items, func(a, b ordering.Purchase) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return ordering.PurchasePage{Items: items, Page: page, PageSize: pageSize, Total: len(items)}, nil
}

var errBoom = errors.New("boom: secret=hunter2")

func money(s string) decimal.Decimal { return decimal.RequireFromString(s) }
