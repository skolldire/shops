//go:build integration

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type browser struct {
	t      *testing.T
	c      client
	http   *http.Client
	origin string
}

func newBrowser(c client) *browser {
	c.t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(c.t, err)
	return &browser{t: c.t, c: c, http: &http.Client{Jar: jar}, origin: c.base}
}

func (b *browser) get(path string) (*http.Response, string) {
	b.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, b.c.base+path, nil)
	require.NoError(b.t, err)
	return b.send(req)
}

func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	b.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, b.c.base+path, strings.NewReader(form.Encode()))
	require.NoError(b.t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if b.origin != "" {
		req.Header.Set("Origin", b.origin)
	}
	return b.send(req)
}

func (b *browser) send(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.http.Do(req)
	require.NoError(b.t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(b.t, err)
	return resp, string(body)
}

func (b *browser) as(user string) {
	b.t.Helper()
	resp, _ := b.post("/demo-user", url.Values{"user": {user}, "next": {"/cart"}})
	require.Equal(b.t, http.StatusOK, resp.StatusCode)
	require.Equal(b.t, "/cart", resp.Request.URL.Path)
}

func (b *browser) add(id string, qty string) {
	b.t.Helper()
	resp, body := b.post("/cart/items", url.Values{"product_id": {id}, "quantity": {qty}})
	require.Equal(b.t, http.StatusOK, resp.StatusCode, body)
	require.Contains(b.t, body, "The product was added to your cart.")
}

var checkoutKey = regexp.MustCompile(`name="idempotency_key" value="([^"]+)"`)

func (b *browser) openCheckout() string {
	b.t.Helper()
	resp, body := b.get("/checkout")
	require.Equal(b.t, http.StatusOK, resp.StatusCode, body)
	m := checkoutKey.FindStringSubmatch(body)
	require.Len(b.t, m, 2)
	return m[1]
}

func (c client) cartLines(user string) int {
	c.t.Helper()
	return c.scalar(`SELECT count(*) FROM cart_items WHERE user_id = $1`, user)
}

func TestAC12CheckoutFromTheUIConfirmsAndEmptiesTheCart(t *testing.T) {
	c := startApp(t)
	a, b := c.product("UI-A", "10.00", 5), c.product("UI-B", "2.50", 5)
	br := newBrowser(c)
	br.as("bob")
	br.add(a, "2")
	br.add(b, "1")
	_, cart := br.get("/cart")
	require.Contains(t, cart, "Estimated total: <strong>$22.50</strong>")
	_, page := br.get("/")
	require.Contains(t, page, ">Cart (2)</a>")

	key := br.openCheckout()
	resp, body := br.post("/checkout", url.Values{"idempotency_key": {key}, "payment_token": {"pm_approved"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.True(t, strings.HasPrefix(resp.Request.URL.Path, "/purchases/"))
	require.Contains(t, body, "Payment approved. Your purchase is confirmed.")
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM purchases WHERE user_id = 'bob' AND status = 'CONFIRMED'`))
	require.Zero(t, c.cartLines("bob"))
	require.Equal(t, []int{3, 4}, []int{c.stockOf(a), c.stockOf(b)})

	again, againBody := br.post("/checkout", url.Values{"idempotency_key": {key}, "payment_token": {"pm_approved"}})
	require.Equal(t, http.StatusOK, again.StatusCode)
	require.Equal(t, "/cart", again.Request.URL.Path, "resubmitting after the cart was emptied asks for a new order")
	require.Contains(t, againBody, "Your cart is empty.")
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM purchases`))

	_, history := br.get("/purchases")
	require.Contains(t, history, resp.Request.URL.Path)
	alice := newBrowser(c)
	resp, _ = alice.get(resp.Request.URL.Path)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "the first demo user cannot see bob's purchase")
}

func TestAC11DeclinedPaymentFromTheUIKeepsTheCart(t *testing.T) {
	c := startApp(t)
	a, b := c.product("UI-C", "10.00", 5), c.product("UI-D", "2.50", 5)
	br := newBrowser(c)
	br.as("carol")
	br.add(a, "2")
	br.add(b, "1")

	key := br.openCheckout()
	resp, body := br.post("/checkout", url.Values{"idempotency_key": {key}, "payment_token": {"pm_declined"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, "The payment was declined (card_declined)")
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM purchases WHERE user_id = 'carol' AND status = 'FAILED'`))
	require.Equal(t, []int{5, 5}, []int{c.stockOf(a), c.stockOf(b)})
	require.Equal(t, 2, c.cartLines("carol"))

	resp, body = br.post("/checkout", url.Values{"idempotency_key": {key}, "payment_token": {"pm_declined"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "The payment was declined", "a double submit shows the same purchase")
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM purchases`))
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM inventory_movements WHERE reason = 'sale_reversal' AND product_id = $1::uuid`, a))
}

func TestAC05PriceChangeIsShownInTheCart(t *testing.T) {
	c := startApp(t)
	id := c.product("UI-E", "29.99", 5)
	br := newBrowser(c)
	br.add(id, "1")
	key := br.openCheckout()

	resp, body := c.do(http.MethodPatch, "/api/v1/products/"+id, `{"price":"31.50"}`, "If-Match", `"1"`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	resp, body = br.post("/checkout", url.Values{"idempotency_key": {key}, "payment_token": {"pm_approved"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/cart", resp.Request.URL.Path)
	require.Contains(t, body, "Some prices changed")
	require.Contains(t, body, "The price changed to $31.50.")
	require.Contains(t, body, "Use current price")
	require.Zero(t, c.scalar(`SELECT count(*) FROM purchases`))
	require.Equal(t, 5, c.stockOf(id))

	resp, body = br.post("/cart/items/"+id+"/accept-price", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotContains(t, body, "The price changed")
	require.Contains(t, body, "Estimated total: <strong>$31.50</strong>")

	resp, body = br.post("/checkout", url.Values{"idempotency_key": {br.openCheckout()}, "payment_token": {"pm_approved"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "Payment approved")
}

func TestUIFormsFromAnotherOriginAreRejected(t *testing.T) {
	c := startApp(t)
	id := c.product("UI-F", "1.00", 5)
	attacker := newBrowser(c)
	attacker.origin = "http://evil.example"

	resp, body := attacker.post("/cart/items", url.Values{"product_id": {id}, "quantity": {"1"}})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Contains(t, body, "Request blocked")
	resp, _ = attacker.post("/checkout", url.Values{"idempotency_key": {"6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"}, "payment_token": {"pm_approved"}})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, _ = attacker.post("/admin/products", url.Values{"sku": {"EVIL-1"}, "name": {"x"}, "category": {"x"}, "price": {"1"}, "stock": {"1"}, "weight_kg": {"1"}})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp, _ = attacker.post("/admin/products/"+id+"/delete", url.Values{"version": {"1"}})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	attacker.origin = ""
	resp, _ = attacker.post("/cart/items", url.Values{"product_id": {id}, "quantity": {"1"}})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "a form without Origin or Referer is rejected")

	require.Zero(t, c.scalar(`SELECT count(*) FROM cart_items`))
	require.Zero(t, c.scalar(`SELECT count(*) FROM purchases`))
	require.Zero(t, c.scalar(`SELECT count(*) FROM products WHERE sku = 'EVIL-1'`))
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM products WHERE status = 'ACTIVE'`))
}
