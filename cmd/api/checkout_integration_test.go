//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
)

type purchaseJSON struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	Currency      string  `json:"currency"`
	Total         string  `json:"total"`
	PaymentRef    *string `json:"payment_ref"`
	FailureReason *string `json:"failure_reason"`
	Items         []struct {
		ProductID string `json:"product_id"`
		Quantity  int    `json:"quantity"`
		UnitPrice string `json:"unit_price"`
	} `json:"items"`
}

func (c client) product(sku, price string, stock int) string {
	c.t.Helper()
	resp, body := c.do(http.MethodPost, "/api/v1/products",
		fmt.Sprintf(`{"sku":%q,"name":"Product %s","category":"Tests","price":%q,"stock":%d,"weight_kg":"1"}`, sku, sku, price, stock))
	require.Equal(c.t, http.StatusCreated, resp.StatusCode, body)
	var p struct {
		ID string `json:"id"`
	}
	require.NoError(c.t, json.Unmarshal([]byte(body), &p))
	return p.ID
}

func (c client) stockOf(id string) int {
	c.t.Helper()
	var n int
	require.NoError(c.t, c.pool.QueryRow(context.Background(), `SELECT stock FROM products WHERE id = $1::uuid`, id).Scan(&n))
	return n
}

func (c client) scalar(sql string, args ...any) int {
	c.t.Helper()
	var n int
	require.NoError(c.t, c.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func items(lines ...string) string {
	return `[` + strings.Join(lines, ",") + `]`
}

func item(id string, qty int, price string) string {
	return fmt.Sprintf(`{"product_id":%q,"quantity":%d,"expected_unit_price":%q}`, id, qty, price)
}

func checkoutBody(token string, lines ...string) string {
	return `{"items":` + items(lines...) + `,"payment_token":"` + token + `"}`
}

func (c client) checkout(user, key, body string) (*http.Response, string) {
	c.t.Helper()
	return c.do(http.MethodPost, "/api/v1/checkout", body, "X-Demo-User-ID", user, "Idempotency-Key", key)
}

func decodePurchase(t *testing.T, body string) purchaseJSON {
	t.Helper()
	var p purchaseJSON
	require.NoError(t, json.Unmarshal([]byte(body), &p), body)
	return p
}

func TestAC01NoOversellUnderConcurrentBuyers(t *testing.T) {
	c := startApp(t)
	id := c.product("HOT-1", "9.99", 5)
	users := c.cfg.Demo.Users

	statuses := make([]string, 100)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Go(func() {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.base+"/api/v1/checkout",
				strings.NewReader(checkoutBody("pm_approved", item(id, 1, "9.99"))))
			if err != nil {
				statuses[i] = err.Error()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Demo-User-ID", users[i%len(users)])
			req.Header.Set("Idempotency-Key", uuid.NewString())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses[i] = err.Error()
				return
			}
			defer resp.Body.Close()
			var b struct {
				Status string `json:"status"`
				Code   string `json:"code"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&b)
			statuses[i] = fmt.Sprintf("%d %s%s", resp.StatusCode, b.Status, b.Code)
		})
	}
	wg.Wait()

	counts := map[string]int{}
	for _, s := range statuses {
		counts[s]++
	}
	require.Equal(t, map[string]int{"201 CONFIRMED": 5, "409 insufficient_stock": 95}, counts)
	require.Zero(t, c.stockOf(id))
	require.Equal(t, 5, c.scalar(`SELECT count(*) FROM purchases WHERE status = 'CONFIRMED'`))
	require.Zero(t, c.scalar(`SELECT count(*) FROM purchases WHERE status <> 'CONFIRMED'`))
	c.requireAuditInvariant()
}

func TestAC02RepeatedCheckoutCreatesOnePurchase(t *testing.T) {
	c := startApp(t)
	id := c.product("IDEM-1", "29.99", 20)
	body := checkoutBody("pm_approved", item(id, 2, "29.99"))
	key := uuid.NewString()

	first, firstBody := c.checkout("alice", key, body)
	require.Equal(t, http.StatusCreated, first.StatusCode, firstBody)
	p := decodePurchase(t, firstBody)
	require.Equal(t, "CONFIRMED", p.Status)
	require.Equal(t, "/api/v1/purchases/"+p.ID, first.Header.Get("Location"))

	again, againBody := c.checkout("alice", key, checkoutBody("pm_approved", item(strings.ToUpper(id), 2, "29.990")))
	require.Equal(t, http.StatusOK, again.StatusCode)
	require.Equal(t, firstBody, againBody, "the repetition answers the same body")
	require.Equal(t, 18, c.stockOf(id))

	parallelKey := uuid.NewString()
	codes := make([]int, 10)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			resp, _ := c.checkout("bob", parallelKey, body)
			codes[i] = resp.StatusCode
		})
	}
	wg.Wait()
	created := 0
	for _, code := range codes {
		require.Contains(t, []int{http.StatusCreated, http.StatusOK, http.StatusAccepted}, code)
		if code == http.StatusCreated {
			created++
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM purchases WHERE user_id = 'bob'`))
	require.Equal(t, 16, c.stockOf(id), "the stock is taken once per key")
	require.Equal(t, 2, c.scalar(`SELECT count(*) FROM inventory_movements WHERE reason = 'sale'`))

	resp, carolBody := c.checkout("carol", key, body)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "the key is scoped by user")
	require.NotEqual(t, p.ID, decodePurchase(t, carolBody).ID)
	c.requireAuditInvariant()
}

func TestAC03SameKeyOtherBodyIsAConflict(t *testing.T) {
	c := startApp(t)
	id := c.product("IDEM-2", "10.00", 10)
	key := uuid.NewString()
	resp, body := c.checkout("alice", key, checkoutBody("pm_approved", item(id, 1, "10.00")))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)

	resp, conflict := c.checkout("alice", key, checkoutBody("pm_approved", item(id, 2, "10.00")))
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	code, _ := errorCodes(t, conflict)
	require.Equal(t, "idempotency_conflict", code)
	require.Equal(t, 9, c.stockOf(id))
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM purchases`))

	_, stored := c.do(http.MethodGet, "/api/v1/purchases/"+decodePurchase(t, body).ID, "", "X-Demo-User-ID", "alice")
	require.Equal(t, body, stored)
}

func TestAC04OneItemWithoutStockChangesNothing(t *testing.T) {
	c := startApp(t)
	a, b, empty := c.product("MIX-A", "1.00", 5), c.product("MIX-B", "2.00", 5), c.product("MIX-C", "3.00", 0)
	resp, body := c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(a, 1, "1.00"), item(b, 2, "2.00"), item(empty, 1, "3.00")))
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	code, fields := errorCodes(t, body)
	require.Equal(t, "insufficient_stock", code)
	require.Equal(t, map[string]string{"items[2].quantity": "insufficient_stock"}, fields)
	require.Contains(t, body, "only 0 available")
	require.Equal(t, []int{5, 5, 0}, []int{c.stockOf(a), c.stockOf(b), c.stockOf(empty)})
	require.Zero(t, c.scalar(`SELECT count(*) FROM purchases`))
	require.Zero(t, c.scalar(`SELECT count(*) FROM inventory_movements WHERE reason = 'sale'`))
}

func TestAC05ChangedPriceIsRejectedWithTheCurrentPrice(t *testing.T) {
	c := startApp(t)
	id := c.product("PRICE-1", "29.99", 5)
	resp, body := c.do(http.MethodPatch, "/api/v1/products/"+id, `{"price":"31.50"}`, "If-Match", `"1"`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	resp, body = c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(id, 1, "29.99")))
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	code, fields := errorCodes(t, body)
	require.Equal(t, "price_changed", code)
	require.Equal(t, map[string]string{"items[0].expected_unit_price": "price_changed"}, fields)
	require.Contains(t, body, "the current price is 31.50")
	require.Equal(t, 5, c.stockOf(id))
	require.Zero(t, c.scalar(`SELECT count(*) FROM purchases`))
}

func TestAC06DeclinedPaymentReturnsTheStockOnce(t *testing.T) {
	c := startApp(t)
	id := c.product("DECL-1", "5.00", 4)
	key := uuid.NewString()
	resp, body := c.checkout("alice", key, checkoutBody("pm_declined", item(id, 3, "5.00")))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	p := decodePurchase(t, body)
	require.Equal(t, "FAILED", p.Status)
	require.Equal(t, "card_declined", *p.FailureReason)
	require.Nil(t, p.PaymentRef)
	require.Equal(t, 4, c.stockOf(id))

	resp, again := c.checkout("alice", key, checkoutBody("pm_declined", item(id, 3, "5.00")))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, body, again)
	require.Equal(t, 4, c.stockOf(id))
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM inventory_movements WHERE reason = 'reservation_release'`))
	require.Zero(t, c.scalar(`SELECT count(*) FROM inventory_movements WHERE reason = 'sale'`))

	resp, body = c.checkout("alice", uuid.NewString(), checkoutBody("pm_other", item(id, 1, "5.00")))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, "unknown_payment_token", *decodePurchase(t, body).FailureReason)
	c.requireAuditInvariant()
}

func TestAC08DeletedProductIsUnavailable(t *testing.T) {
	c := startApp(t)
	id := c.product("GONE-1", "7.00", 5)
	resp, _ := c.do(http.MethodDelete, "/api/v1/products/"+id, "", "If-Match", `"1"`)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	_, store := c.do(http.MethodGet, "/", "")
	require.NotContains(t, store, "Product GONE-1")
	_, search := c.do(http.MethodGet, "/api/v1/products?q=GONE", "")
	require.Contains(t, search, `"total":0`)

	resp, body := c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(id, 1, "7.00")))
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	code, fields := errorCodes(t, body)
	require.Equal(t, "product_unavailable", code)
	require.Equal(t, map[string]string{"items[0].product_id": "product_unavailable"}, fields)
	require.Zero(t, c.scalar(`SELECT count(*) FROM purchases`))
}

func TestPatchWithAnETagFromBeforeASaleIsRejected(t *testing.T) {
	c := startApp(t)
	id := c.product("ETAG-1", "3.00", 5)
	resp, _ := c.do(http.MethodGet, "/api/v1/products/"+id, "")
	require.Equal(t, `"1"`, resp.Header.Get("ETag"))

	resp, body := c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(id, 2, "3.00")))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)

	resp, body = c.do(http.MethodPatch, "/api/v1/products/"+id, `{"stock":5}`, "If-Match", `"1"`)
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
	code, _ := errorCodes(t, body)
	require.Equal(t, "version_conflict", code)
	require.Equal(t, 3, c.stockOf(id), "the sale is not overwritten")
}

func TestPurchaseQueriesOverHTTP(t *testing.T) {
	c := startApp(t)
	id := c.product("Q-1", "4.00", 5)
	_, body := c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(id, 1, "4.00")))
	p := decodePurchase(t, body)

	resp, got := c.do(http.MethodGet, "/api/v1/purchases/"+p.ID, "", "X-Demo-User-ID", "alice")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, body, got)
	require.NotContains(t, got, "pm_approved")

	for _, user := range []string{"bob", "carol"} {
		resp, got = c.do(http.MethodGet, "/api/v1/purchases/"+p.ID, "", "X-Demo-User-ID", user)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		code, _ := errorCodes(t, got)
		require.Equal(t, "not_found", code)
	}
	resp, _ = c.do(http.MethodGet, "/api/v1/purchases/"+p.ID, "")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, got = c.do(http.MethodGet, "/api/v1/purchases?page_size=1", "", "X-Demo-User-ID", "alice")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, got, `"total":1`)
	require.Contains(t, got, `"page_size":1`)
	_, got = c.do(http.MethodGet, "/api/v1/purchases", "", "X-Demo-User-ID", "bob")
	require.JSONEq(t, `{"items":[],"page":1,"page_size":20,"total":0}`, got)
}

func TestCheckoutRejectsInvalidUUIDs(t *testing.T) {
	c := startApp(t)
	id := c.product("UUID-1", "4.00", 5)
	for _, key := range []string{"", "nope", "{" + uuid.NewString() + "}", strings.ReplaceAll(uuid.NewString(), "-", "")} {
		resp, body := c.checkout("alice", key, checkoutBody("pm_approved", item(id, 1, "4.00")))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, key)
		code, _ := errorCodes(t, body)
		require.Equal(t, "invalid_idempotency_key", code)
	}
	resp, body := c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item("urn:uuid:"+id, 1, "4.00")))
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	_, fields := errorCodes(t, body)
	require.Equal(t, map[string]string{"items[0].product_id": "invalid_format"}, fields)
	require.Equal(t, 5, c.stockOf(id))
}

func TestAuditInvariantAfterImportAndPurchases(t *testing.T) {
	c := startApp(t)
	official, err := os.ReadFile(officialCSV)
	require.NoError(t, err)
	c.importReport(official)
	var id, price string
	require.NoError(t, c.pool.QueryRow(context.Background(),
		`SELECT id::text, price::text FROM products WHERE stock >= 3 ORDER BY sku LIMIT 1`).Scan(&id, &price))

	key := uuid.NewString()
	for range 2 {
		c.checkout("alice", key, checkoutBody("pm_approved", item(id, 1, price)))
	}
	c.checkout("bob", uuid.NewString(), checkoutBody("pm_declined", item(id, 1, price)))
	c.product("ZERO-1", "1.00", 0)
	manual := c.product("MAN-1", "1.00", 3)
	resp, _ := c.do(http.MethodPatch, "/api/v1/products/"+manual, `{"stock":8}`, "If-Match", `"1"`)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	c.requireAuditInvariant()
	require.Zero(t, c.scalar(`SELECT count(*) FROM inventory_movements m WHERE reason = 'import_replace'
		AND NOT EXISTS (SELECT 1 FROM imports i WHERE i.id::text = m.reference AND i.file_hash = $1 AND i.status = 'APPLIED')`, officialSHA256))
	imported := c.scalar(`SELECT count(*) FROM products WHERE category <> 'Tests'`)
	zeroStock := c.scalar(`SELECT count(*) FROM products WHERE category <> 'Tests' AND stock = 0`)
	require.Equal(t, 82, imported)
	require.Equal(t, imported-zeroStock, c.scalar(`SELECT count(*) FROM inventory_movements WHERE reason = 'import_replace'`),
		"one import movement per created product with stock, none for stock 0")
	require.Zero(t, c.scalar(`SELECT count(*) FROM inventory_movements m JOIN products p ON p.id = m.product_id WHERE p.sku = 'ZERO-1'`))
	require.Equal(t, 1, c.scalar(`SELECT count(*) FROM inventory_movements WHERE reason = 'manual_adjustment' AND delta = 5 AND reference IS NULL`))
}

func (c client) requireAuditInvariant() {
	c.t.Helper()
	require.Zero(c.t, c.scalar(`
		SELECT count(*) FROM products p
		WHERE p.stock <> (SELECT COALESCE(sum(m.delta), 0) FROM inventory_movements m WHERE m.product_id = p.id)
		   OR p.reserved <> (SELECT COALESCE(sum(m.reserved_delta), 0) FROM inventory_movements m WHERE m.product_id = p.id)`),
		"every product satisfies stock = sum(delta) and reserved = sum(reserved_delta)")
	require.Zero(c.t, c.scalar(`SELECT count(*) FROM inventory_movements WHERE reason = 'unattributed'`))
	require.Zero(c.t, c.scalar(`
		SELECT count(*) FROM inventory_movements m
		WHERE NOT (
			(m.reason IN ('initial_stock', 'manual_adjustment') AND m.reference IS NULL)
			OR (m.reason IN ('import_replace', 'import_add') AND EXISTS (SELECT 1 FROM imports i WHERE i.id::text = m.reference AND i.mode = substr(m.reason, 8)))
			OR (m.reason IN ('reservation', 'sale', 'reservation_release') AND EXISTS (SELECT 1 FROM purchases p WHERE p.id::text = m.reference))
		)`), "every movement carries the reason and reference of its writer")
	require.Zero(c.t, c.scalar(`
		SELECT count(*) FROM inventory_movements m
		WHERE (m.reason = 'reservation_release' AND NOT EXISTS (SELECT 1 FROM purchases p WHERE p.id::text = m.reference AND p.status = 'FAILED'))
		   OR (m.reason = 'sale' AND NOT EXISTS (SELECT 1 FROM purchases p WHERE p.id::text = m.reference AND p.status = 'CONFIRMED'))`),
		"a release belongs to a declined purchase and a sale to a confirmed one")
}

func TestAMalformedOrMissingExpectedPriceChangesNothing(t *testing.T) {
	c := startApp(t)
	id := c.product("FMT-1", "10.00", 5)
	require.NoError(t, c.app.ordering.AddToCart(t.Context(), "alice", id, 2))
	movements := c.scalar(`SELECT count(*) FROM inventory_movements`)
	unchanged := func(t *testing.T) {
		t.Helper()
		require.Zero(t, c.scalar(`SELECT count(*) FROM purchases`), "no purchase")
		require.Zero(t, c.scalar(`SELECT count(*) FROM purchase_items`), "no purchase item")
		require.Zero(t, c.scalar(`SELECT reserved FROM products WHERE id = $1::uuid`, id), "nothing reserved")
		require.Equal(t, movements, c.scalar(`SELECT count(*) FROM inventory_movements`), "no movement")
		require.Zero(t, c.scalar(`SELECT count(*) FROM payment_fake_charges`), "the provider was not asked to charge")
		require.Equal(t, 2, c.scalar(`SELECT quantity FROM cart_items WHERE user_id = 'alice' AND product_id = $1::uuid`, id), "the cart is untouched")
	}
	invalidCount := func() string {
		rec := httptest.NewRecorder()
		c.app.metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		for line := range strings.Lines(rec.Body.String()) {
			if strings.HasPrefix(line, `checkout_total{outcome="invalid"}`) {
				return strings.TrimSpace(line)
			}
		}
		return ""
	}

	resp, body := c.checkout("alice", uuid.NewString(), `{"items":[{"product_id":"`+id+`","quantity":2,"expected_unit_price":"abc"}],"payment_token":"pm_approved"}`)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	code, fields := errorCodes(t, body)
	require.Equal(t, "validation_failed", code)
	require.Equal(t, map[string]string{"items[0].expected_unit_price": "invalid_format"}, fields)
	unchanged(t)
	require.Equal(t, `checkout_total{outcome="invalid"} 0`, invalidCount(), "a malformed body never reaches the checkout use case")

	resp, body = c.checkout("alice", uuid.NewString(), `{"items":[{"product_id":"`+id+`","quantity":2}],"payment_token":"pm_approved"}`)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	code, fields = errorCodes(t, body)
	require.Equal(t, "validation_failed", code)
	require.Equal(t, map[string]string{"items[0].expected_unit_price": "required"}, fields, "the expected price is required by the contract")
	unchanged(t)
	require.Equal(t, `checkout_total{outcome="invalid"} 1`, invalidCount(), "a well-formed body is validated by ordering")

	resp, body = c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(id, 2, "10.00")))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	require.Equal(t, 3, c.stockOf(id))
}

func TestACheckoutThatWaitsTooLongForAnAddImportIsTryAgain(t *testing.T) {
	c := startApp(t)
	ctx := t.Context()
	low, high := "00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000a2"
	_, err := c.pool.Exec(ctx, `INSERT INTO products (id, sku, name, category, price, stock, weight_kg)
		VALUES ($1::uuid, 'LOCK-A', 'Locked by the import', 'Tests', 10.00, 5, 1), ($2::uuid, 'LOCK-B', 'Held by the test', 'Tests', 10.00, 5, 1)`, low, high)
	require.NoError(t, err)
	require.NoError(t, c.app.ordering.AddToCart(ctx, "alice", low, 2))
	movements := c.scalar(`SELECT count(*) FROM inventory_movements`)

	holder, err := c.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(context.Background()) }()
	_, err = holder.Exec(ctx, `SELECT 1 FROM products WHERE id = $1::uuid FOR UPDATE`, high)
	require.NoError(t, err)

	imported := make(chan error, 1)
	go func() {
		_, err := c.app.catalog.Import(context.Background(), strings.NewReader(importHeader+"A,LOCK-A,,Tests,10.00,3,1\nB,LOCK-B,,Tests,10.00,3,1\n"),
			catalog.ImportOptions{FileName: "delivery.csv", Mode: catalog.ImportAdd, IdempotencyKey: uuid.NewString()})
		imported <- err
	}()
	require.Eventually(t, func() bool {
		return c.scalar(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%ORDER BY id FOR UPDATE%'`) == 1
	}, 10*time.Second, 10*time.Millisecond, "the add import holds LOCK-A and waits for LOCK-B")

	resp, body := c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(low, 2, "10.00")))
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, body)
	require.Equal(t, "1", resp.Header.Get("Retry-After"))
	code, _ := errorCodes(t, body)
	require.Equal(t, "try_again", code)

	require.Zero(t, c.scalar(`SELECT count(*) FROM purchases`), "no purchase")
	require.Zero(t, c.scalar(`SELECT count(*) FROM purchase_items`), "no purchase item")
	require.Zero(t, c.scalar(`SELECT reserved FROM products WHERE id = $1::uuid`, low), "nothing reserved")
	require.Equal(t, movements, c.scalar(`SELECT count(*) FROM inventory_movements`), "no movement while the import is still waiting")
	require.Zero(t, c.scalar(`SELECT count(*) FROM payment_fake_charges`), "no charge")
	require.Equal(t, 2, c.scalar(`SELECT quantity FROM cart_items WHERE user_id = 'alice' AND product_id = $1::uuid`, low), "the cart is untouched")

	require.NoError(t, holder.Rollback(ctx))
	require.NoError(t, <-imported, "the import that was already running finishes")
	require.Equal(t, 2, c.scalar(`SELECT count(*) FROM inventory_movements WHERE reason = 'import_add'`), "its own movements are the import's")
	require.Zero(t, c.scalar(`SELECT count(*) FROM inventory_movements WHERE reason = 'reservation'`), "the rejected checkout left none")
	require.Equal(t, 8, c.stockOf(low))
}
