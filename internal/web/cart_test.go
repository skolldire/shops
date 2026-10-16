package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
	"github.com/skolldire/shops/internal/platform/validation"
)

func stockedShoes(stock int) map[string]catalog.Product {
	return map[string]catalog.Product{"p1": {ID: "p1", SKU: "RS-001", Name: "Running Shoes", Price: money("29.99"), Stock: stock + 2, Reserved: 2, Available: stock, WeightKg: money("1")}}
}

func TestProductPageOffersTheCartOnlyWithStock(t *testing.T) {
	h, _ := newOrderingUI(t, &fakeOrdering{}, &fakeCatalog{products: stockedShoes(3)})
	body := get(h, "/products/p1").Body.String()
	require.Contains(t, body, `action="/cart/items"`)
	require.Contains(t, body, `name="product_id" value="p1"`)

	h, _ = newOrderingUI(t, &fakeOrdering{}, &fakeCatalog{products: stockedShoes(0)})
	body = get(h, "/products/p1").Body.String()
	require.NotContains(t, body, "Add to cart")
	require.Contains(t, body, "Out of stock")
}

func TestAddToCart(t *testing.T) {
	orders := &fakeOrdering{}
	h, _ := newOrderingUI(t, orders, &fakeCatalog{products: stockedShoes(3)})
	rec := submit(h, "/cart/items", url.Values{"product_id": {"p1"}, "quantity": {"2"}}, as("bob")...)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/cart?notice=added", rec.Header().Get("Location"))
	require.Equal(t, []cartCall{{op: "add", user: "bob", product: "p1", quantity: 2}}, orders.calls)

	for _, qty := range []string{"0", "100", "x", "1.5"} {
		orders.writeErr = validation.Errors{{Field: "quantity", Code: "out_of_range", Message: "must be between 1 and 99"}}
		rec = submit(h, "/cart/items", url.Values{"product_id": {"p1"}, "quantity": {qty}})
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, qty)
		require.Contains(t, rec.Body.String(), "between 1 and 99")
	}
	require.Len(t, orders.calls, 3)
	require.Equal(t, 0, orders.calls[1].quantity)
	require.Equal(t, 100, orders.calls[2].quantity)

	orders.writeErr = ordering.ErrCartFull
	rec = submit(h, "/cart/items", url.Values{"product_id": {"p1"}, "quantity": {"1"}})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Contains(t, rec.Body.String(), "already has 50 different products")

	orders.writeErr = ordering.ErrProductUnavailable
	rec = submit(h, "/cart/items", url.Values{"product_id": {"p1"}, "quantity": {"1"}})
	require.Equal(t, "/cart?notice=missing", rec.Header().Get("Location"))

	orders.writeErr = errBoom
	rec = submit(h, "/cart/items", url.Values{"product_id": {"p1"}, "quantity": {"1"}})
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "hunter2")
}

func TestCartChanges(t *testing.T) {
	orders := &fakeOrdering{}
	h, _ := newOrderingUI(t, orders, nil)
	rec := submit(h, "/cart/items/p1/quantity", url.Values{"quantity": {"4"}}, as("carol")...)
	require.Equal(t, "/cart?notice=updated", rec.Header().Get("Location"))
	orders.writeErr = validation.Errors{{Field: "quantity", Code: "out_of_range", Message: "must be between 1 and 99"}}
	rec = submit(h, "/cart/items/p1/quantity", url.Values{"quantity": {"0"}})
	require.Equal(t, "/cart?notice=quantity", rec.Header().Get("Location"))
	orders.writeErr = nil
	rec = submit(h, "/cart/items/p1/remove", nil)
	require.Equal(t, "/cart?notice=removed", rec.Header().Get("Location"))
	rec = submit(h, "/cart/items/p1/accept-price", nil)
	require.Equal(t, "/cart?notice=price_accepted", rec.Header().Get("Location"))
	require.Equal(t, []cartCall{
		{op: "quantity", user: "carol", product: "p1", quantity: 4},
		{op: "quantity", user: "alice", product: "p1", quantity: 0},
		{op: "remove", user: "alice", product: "p1"},
		{op: "accept", user: "alice", product: "p1"},
	}, orders.calls)

	orders.writeErr = ordering.ErrProductUnavailable
	require.Equal(t, "/cart?notice=missing", submit(h, "/cart/items/p1/accept-price", nil).Header().Get("Location"))
	orders.writeErr = validation.Errors{{Field: "quantity", Code: "out_of_range", Message: "x"}}
	require.Equal(t, "/cart?notice=quantity", submit(h, "/cart/items/p1/quantity", url.Values{"quantity": {"3"}}).Header().Get("Location"))
}

func warnedCart() ordering.Cart {
	return ordering.Cart{
		Lines: []ordering.CartLine{
			{ProductID: "p1", Name: "Running Shoes", Quantity: 2, UnitPriceSeen: money("29.99"), CurrentPrice: money("31.50"), Available: 5, PriceChanged: true},
			{ProductID: "p2", Name: "Socks", Quantity: 4, UnitPriceSeen: money("5.00"), CurrentPrice: money("5.00"), Available: 1, LowStock: true},
			{ProductID: "p3", Quantity: 1, UnitPriceSeen: money("2.00"), Unavailable: true},
		},
		EstimatedTotal: money("81.98"), HasWarnings: true,
	}
}

func TestCartPageShowsWarnings(t *testing.T) {
	h, _ := newOrderingUI(t, &fakeOrdering{cart: warnedCart()}, nil)
	rec := get(h, "/cart?notice=price_changed")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "Some prices changed")
	require.Contains(t, body, "The price changed to $31.50.")
	require.Contains(t, body, `action="/cart/items/p1/accept-price"`)
	require.Contains(t, body, "Use current price")
	require.Contains(t, body, "Only 1 in stock.")
	require.Contains(t, body, "This product is no longer available.")
	require.Contains(t, body, "Estimated total: <strong>$81.98</strong>")
	require.NotContains(t, body, `href="/checkout"`)
	requireNoInlineCode(t, body)

	h, _ = newOrderingUI(t, &fakeOrdering{}, nil)
	require.Contains(t, get(h, "/cart").Body.String(), "Your cart is empty.")
}

func readyCart() ordering.Cart {
	return ordering.Cart{Lines: []ordering.CartLine{
		{ProductID: "p1", Name: "Running Shoes", Quantity: 2, UnitPriceSeen: money("29.99"), CurrentPrice: money("29.99"), Available: 5},
	}, EstimatedTotal: money("59.98")}
}

var keyField = regexp.MustCompile(`name="idempotency_key" value="([^"]+)"`)

func keyOf(t *testing.T, body string) string {
	t.Helper()
	m := keyField.FindStringSubmatch(body)
	require.Len(t, m, 2, body)
	_, err := uuid.Parse(m[1])
	require.NoError(t, err)
	return m[1]
}

func TestCheckoutPageGeneratesAKeyPerAttempt(t *testing.T) {
	h, _ := newOrderingUI(t, &fakeOrdering{cart: readyCart()}, nil)
	first := get(h, "/checkout")
	require.Equal(t, http.StatusOK, first.Code)
	body := first.Body.String()
	require.Contains(t, body, `<option value="pm_approved" selected>`)
	require.Contains(t, body, `<option value="pm_declined"`)
	require.Contains(t, body, `hx-disabled-elt="find button"`)
	requireNoInlineCode(t, body)
	require.NotEqual(t, keyOf(t, body), keyOf(t, get(h, "/checkout").Body.String()))

	h, _ = newOrderingUI(t, &fakeOrdering{}, nil)
	require.Equal(t, "/cart?notice=empty", get(h, "/checkout").Header().Get("Location"))
	h, _ = newOrderingUI(t, &fakeOrdering{cart: warnedCart()}, nil)
	require.Equal(t, "/cart", get(h, "/checkout").Header().Get("Location"))
}

func TestCheckoutResults(t *testing.T) {
	key := uuid.NewString()
	form := url.Values{"idempotency_key": {key}, "payment_token": {"pm_declined"}}
	price := &ordering.RejectedError{Reason: ordering.ErrPriceChanged}
	tests := []struct {
		name     string
		result   ordering.CheckoutResult
		err      error
		status   int
		location string
	}{
		{"confirmed", ordering.CheckoutResult{Purchase: ordering.Purchase{ID: "x1", Status: ordering.StatusConfirmed}, Kind: ordering.Created}, nil, http.StatusSeeOther, "/purchases/x1"},
		{"failed", ordering.CheckoutResult{Purchase: ordering.Purchase{ID: "x2", Status: ordering.StatusFailed}, Kind: ordering.Created}, nil, http.StatusSeeOther, "/purchases/x2"},
		{"pending", ordering.CheckoutResult{Purchase: ordering.Purchase{ID: "x3", Status: ordering.StatusPending}, Kind: ordering.Created}, nil, http.StatusSeeOther, "/purchases/x3"},
		{"replayed double click", ordering.CheckoutResult{Purchase: ordering.Purchase{ID: "x1", Status: ordering.StatusConfirmed}, Kind: ordering.Replayed}, nil, http.StatusSeeOther, "/purchases/x1"},
		{"price changed", ordering.CheckoutResult{}, price, http.StatusSeeOther, "/cart?notice=price_changed"},
		{"stock", ordering.CheckoutResult{}, &ordering.RejectedError{Reason: ordering.ErrInsufficientStock}, http.StatusSeeOther, "/cart?notice=stock"},
		{"unavailable", ordering.CheckoutResult{}, &ordering.RejectedError{Reason: ordering.ErrProductUnavailable}, http.StatusSeeOther, "/cart?notice=unavailable"},
		{"empty cart", ordering.CheckoutResult{}, validation.Errors{{Field: "items", Code: "required", Message: "x"}}, http.StatusSeeOther, "/cart?notice=empty"},
		{"another rejection is not shown as unavailable", ordering.CheckoutResult{}, &ordering.RejectedError{Reason: errors.New("ordering: a reason the UI does not know"),
			Errors: validation.Errors{{Field: "items[0].quantity", Code: "x", Message: "x"}}}, http.StatusInternalServerError, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			orders := &fakeOrdering{cart: readyCart(), result: tc.result, checkout: tc.err}
			h, _ := newOrderingUI(t, orders, nil)
			rec := submit(h, "/checkout", form, as("bob")...)
			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, tc.location, rec.Header().Get("Location"))
			require.Equal(t, []string{key}, orders.keys)
			require.Equal(t, []string{"pm_declined"}, orders.tokens)
		})
	}
}

func TestCheckoutConflictRegeneratesTheKeyAndTryAgainKeepsIt(t *testing.T) {
	key := uuid.NewString()
	orders := &fakeOrdering{cart: readyCart(), checkout: ordering.ErrIdempotencyConflict}
	h, _ := newOrderingUI(t, orders, nil)
	rec := submit(h, "/checkout", url.Values{"idempotency_key": {key}, "payment_token": {"pm_approved"}})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "Your cart changed")
	require.NotEqual(t, key, keyOf(t, rec.Body.String()))

	orders.checkout = ordering.ErrTryAgain
	rec = submit(h, "/checkout", url.Values{"idempotency_key": {key}, "payment_token": {"pm_approved"}})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "1", rec.Header().Get("Retry-After"))
	require.Equal(t, key, keyOf(t, rec.Body.String()))

	orders.checkout = ordering.ErrInvalidIdempotencyKey
	rec = submit(h, "/checkout", url.Values{"idempotency_key": {"forged"}, "payment_token": {"pm_approved"}})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	keyOf(t, rec.Body.String())

	orders.checkout = errBoom
	rec = submit(h, "/checkout", url.Values{"idempotency_key": {key}, "payment_token": {"pm_approved"}})
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "hunter2")
}

func purchaseOf(id string, status ordering.Status) ordering.Purchase {
	p := ordering.Purchase{
		ID: id, Status: status, Currency: "USD", Total: money("59.98"), CreatedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
		Items: []ordering.PurchaseItem{{ProductID: "p1", SKU: "RS-001", Name: "Running Shoes", UnitPrice: money("29.99"), Quantity: 2, LineTotal: money("59.98")}},
	}
	switch status {
	case ordering.StatusConfirmed:
		p.PaymentRef = "fake_" + id
	case ordering.StatusFailed:
		p.FailureReason = "card_declined"
	}
	return p
}

func TestPurchasePages(t *testing.T) {
	orders := &fakeOrdering{purchases: map[string]ordering.Purchase{
		"alice/c1": purchaseOf("c1", ordering.StatusConfirmed),
		"alice/f1": purchaseOf("f1", ordering.StatusFailed),
		"alice/p1": purchaseOf("p1", ordering.StatusPending),
		"bob/b1":   purchaseOf("b1", ordering.StatusConfirmed),
	}}
	h, _ := newOrderingUI(t, orders, nil)

	body := get(h, "/purchases/c1").Body.String()
	require.Contains(t, body, "Payment approved. Your purchase is confirmed.")
	require.Contains(t, body, "fake_c1")
	require.Contains(t, body, "$59.98")

	body = get(h, "/purchases/f1").Body.String()
	require.Contains(t, body, "The payment was declined (card_declined)")
	require.Contains(t, body, "your cart was kept")
	require.NotContains(t, body, "confirmed")

	body = get(h, "/purchases/p1").Body.String()
	require.Contains(t, body, "still pending")
	require.Contains(t, body, `href="/purchases"`)
	require.NotContains(t, body, "Payment approved")

	require.Equal(t, http.StatusNotFound, get(h, "/purchases/b1").Code, "another user's purchase is not found")
	require.Equal(t, http.StatusOK, get(h, "/purchases/b1", as("bob")...).Code)

	body = get(h, "/purchases?page=x").Body.String()
	require.Equal(t, []int{0}, orders.listed)
	require.Equal(t, 3, strings.Count(body, `href="/purchases/`))
	require.Contains(t, body, "Pending")
	require.Contains(t, body, "Declined")
	requireNoInlineCode(t, body)
}

func requireNoInlineCode(t *testing.T, body string) {
	t.Helper()
	require.False(t, inlineScript.MatchString(strings.ReplaceAll(body, `<script src="/static/htmx.min.js" defer></script>`, "")), "inline script")
	require.False(t, inlineStyle.MatchString(body), "inline style or handler")
}

func TestQuantityRangeValidationComesFromOrdering(t *testing.T) {
	for _, quantity := range []string{"0", "-1", "100"} {
		t.Run(quantity, func(t *testing.T) {
			orders := &fakeOrdering{writeErr: validation.Errors{{Field: "quantity", Code: "out_of_range", Message: "quantity rejected by ordering"}}}
			h, _ := newOrderingUI(t, orders, &fakeCatalog{products: stockedShoes(3)})
			rec := submit(h, "/cart/items", url.Values{"product_id": {"p1"}, "quantity": {quantity}})
			require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			require.Contains(t, rec.Body.String(), "quantity rejected by ordering")
			require.Len(t, orders.calls, 1)
			require.Equal(t, "add", orders.calls[0].op)
		})
	}
}

func TestPurchasePagingDelegatesRangeAndDefaultsToOrdering(t *testing.T) {
	orders := &fakeOrdering{}
	h, _ := newOrderingUI(t, orders, nil)
	require.Equal(t, http.StatusOK, get(h, "/purchases").Code)
	require.Equal(t, []int{0}, orders.listed)
	require.Equal(t, []int{0}, orders.sizes)

	orders.listErr = validation.Errors{{Field: "page", Code: "out_of_range", Message: "invalid page"}}
	for _, path := range []string{"/purchases?page=100001&page_size=101", "/purchases?page=-1"} {
		rec := get(h, path)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		require.Contains(t, rec.Body.String(), "page or page size is invalid")
		require.NotContains(t, rec.Body.String(), "You have no purchases yet")
	}
	require.Equal(t, []int{0, 100001, -1}, orders.listed)
	require.Equal(t, []int{0, 101, 0}, orders.sizes)
}

func TestTheCartFullMessageUsesTheOrderingLimit(t *testing.T) {
	require.Equal(t, fmt.Sprintf("Your cart already has %d different products. Remove one before adding another.", ordering.MaxCartLines), cartFullMessage)
}
