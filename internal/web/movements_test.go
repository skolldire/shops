package web

import (
	"net/http"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
)

func TestAdminMovementsPage(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	cat := &fakeCatalog{
		products: map[string]catalog.Product{"p1": {ID: "p1", SKU: "RS-001", Name: "Running Shoes", Price: decimal.RequireFromString("29.99"), Stock: 4}},
		movements: []catalog.Movement{
			{ID: 2, Delta: -1, StockAfter: 4, Reason: "sale", Reference: "purchase-9", DBUser: "shop", CreatedAt: at},
			{ID: 1, Delta: 5, StockAfter: 5, Reason: "initial_stock", DBUser: "shop", CreatedAt: at},
		},
		total: 45,
	}
	h, _ := newTestUI(t, cat)

	rec := get(h, "/admin/products/p1/movements?page=2&page_size=20")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Equal(t, catalog.Paging{Page: 2, PageSize: 20}, cat.paging)
	require.Contains(t, body, "Stock movements of Running Shoes")
	require.Contains(t, body, "purchase-9")
	require.Contains(t, body, "+5")
	require.Contains(t, body, "initial_stock")
	require.Contains(t, body, "2026-10-14 12:30:00 UTC")
	require.Contains(t, body, `href="/admin/products/p1/movements?page=1&amp;page_size=20"`)
	require.Contains(t, body, `href="/admin/products/p1/movements?page=3&amp;page_size=20"`)

	rec = get(h, "/admin/products/p1/movements?page=x")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "The page parameters are not valid.")

	require.Equal(t, http.StatusNotFound, get(h, "/admin/products/missing/movements").Code)
}

func TestAdminListLinksToMovements(t *testing.T) {
	cat := &fakeCatalog{products: map[string]catalog.Product{"p1": {ID: "p1", SKU: "RS-001", Name: "Running Shoes", Price: decimal.RequireFromString("29.99")}}}
	h, _ := newTestUI(t, cat)
	require.Contains(t, get(h, "/admin/products").Body.String(), `href="/admin/products/p1/movements"`)
}
