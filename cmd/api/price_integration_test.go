//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAConfirmedPurchaseKeepsItsPriceAfterTheCatalogChanges(t *testing.T) {
	c := startApp(t)
	id := c.product("KEEP-1", "29.99", 10)
	resp, body := c.checkout("alice", uuid.NewString(), checkoutBody("pm_approved", item(id, 2, "29.99")))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	purchase := decodePurchase(t, body)
	require.Equal(t, "CONFIRMED", purchase.Status)
	path := "/api/v1/purchases/" + purchase.ID
	_, original := c.do(http.MethodGet, path, "", "X-Demo-User-ID", "alice")

	requireUnchanged := func(t *testing.T, current string) {
		t.Helper()
		var price string
		require.NoError(t, c.pool.QueryRow(context.Background(), `SELECT price::text FROM products WHERE id = $1::uuid`, id).Scan(&price))
		require.Equal(t, current, price, "the catalog did change")

		_, api := c.do(http.MethodGet, path, "", "X-Demo-User-ID", "alice")
		require.Equal(t, original, api)
		history := newBrowser(c)
		history.as("alice")
		resp, page := history.get("/purchases/" + purchase.ID)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, page, "$29.99")
		require.Contains(t, page, "$59.98")
		require.NotContains(t, page, current)

		var total, unit, line string
		require.NoError(t, c.pool.QueryRow(context.Background(),
			`SELECT p.total::text, i.unit_price::text, i.line_total::text
			 FROM purchases p JOIN purchase_items i ON i.purchase_id = p.id WHERE p.id = $1::uuid`, purchase.ID).
			Scan(&total, &unit, &line))
		require.Equal(t, []string{"59.98", "29.99", "59.98"}, []string{total, unit, line})
	}

	t.Run("after an import", func(t *testing.T) {
		report := c.importReport([]byte("name,sku,description,category,price,stock,weight_kg\nKept,KEEP-1,Same product,Tests,39.99,10,1\n"))
		require.Equal(t, 1, report.Updated)
		requireUnchanged(t, "39.99")
	})

	t.Run("after an edit", func(t *testing.T) {
		version := c.scalar(`SELECT version FROM products WHERE id = $1::uuid`, id)
		resp, body := c.do(http.MethodPatch, "/api/v1/products/"+id, `{"price":"49.99"}`, "If-Match", fmt.Sprintf(`"%d"`, version))
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		requireUnchanged(t, "49.99")
	})
}
