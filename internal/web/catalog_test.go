package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
)

const productID = "2b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b"

func shoes() catalog.Product {
	return catalog.Product{
		ID: productID, SKU: "RS-001", Name: "Running Shoes", Description: "Light and fast", Category: "Sports",
		Price: decimal.RequireFromString("29.9"), Stock: 3, WeightKg: decimal.RequireFromString("0.85"), Version: 2,
	}
}

func withProducts(products ...catalog.Product) *fakeCatalog {
	cat := &fakeCatalog{products: map[string]catalog.Product{}, categories: []string{"Audio", "Sports"}}
	for _, p := range products {
		cat.products[p.ID] = p
	}
	return cat
}

func TestStoreRendersProductsAndFilters(t *testing.T) {
	h, _ := newTestUI(t, withProducts(shoes()))

	rec := get(h, "/")

	require.Equal(t, http.StatusOK, rec.Code)
	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.Contains(t, body, `<a href="/products/`+productID+`">Running Shoes</a>`)
	require.Contains(t, body, "$29.90")
	require.Contains(t, body, "3 in stock")
	require.Contains(t, body, `<option value="Audio">Audio</option>`)
	require.Contains(t, body, `hx-push-url="true"`)
	require.Contains(t, body, "Showing 1–1 of 1 products")
}

func TestStoreEscapesUserContent(t *testing.T) {
	evil := shoes()
	evil.Name = "<script>alert(1)</script>"
	evil.Category = `"><img src=x onerror=alert(2)>`
	h, _ := newTestUI(t, withProducts(evil))

	for _, path := range []string{"/", "/products/" + productID} {
		rec := get(h, path)

		require.Equal(t, http.StatusOK, rec.Code, path)
		body := rec.Body.String()
		require.NotContains(t, body, "<script>alert(1)</script>", path)
		require.Contains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;", path)
		require.NotContains(t, body, "<img src=x", path)
		requireSafeHTML(t, rec)
	}
}

func TestStoreForwardsFiltersToTheCatalog(t *testing.T) {
	cat := withProducts(shoes())
	h, _ := newTestUI(t, cat)

	rec := get(h, "/?q=50%25&category=Sports&min_price=10&max_price=99.5&in_stock=true&sort=-price&page=2")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "50%", cat.query.Q)
	require.Equal(t, "Sports", cat.query.Category)
	require.Equal(t, "10", cat.query.MinPrice.String())
	require.Equal(t, "99.5", cat.query.MaxPrice.String())
	require.True(t, cat.query.InStock)
	require.Equal(t, "-price", cat.query.Sort)
	require.Equal(t, 2, cat.query.Page)
	body := rec.Body.String()
	require.Contains(t, body, `value="50%"`)
	require.Contains(t, body, `<option value="Sports" selected>Sports</option>`)
	require.Contains(t, body, `<option value="-price" selected>Price (high to low)</option>`)
	require.Contains(t, body, `id="in_stock" name="in_stock" type="checkbox" value="true" checked`)
}

func TestStoreKeepsInStockCheckedForEveryAcceptedValue(t *testing.T) {
	h, _ := newTestUI(t, withProducts(shoes()))

	for _, value := range []string{"true", "1", "t", "TRUE"} {
		body := get(h, "/?in_stock="+value).Body.String()
		require.Contains(t, body, `id="in_stock" name="in_stock" type="checkbox" value="true" checked`, value)
	}
	for _, value := range []string{"false", "0", ""} {
		body := get(h, "/?in_stock="+value).Body.String()
		require.NotContains(t, body, `value="true" checked`, value)
	}
}

func TestStorePaginationKeepsFilters(t *testing.T) {
	cat := withProducts(shoes())
	cat.total = 45
	h, _ := newTestUI(t, cat)

	body := get(h, "/?q=shoes&page=2").Body.String()

	require.Contains(t, body, `href="/?page=1&amp;q=shoes"`)
	require.Contains(t, body, `href="/?page=3&amp;q=shoes"`)
	require.Contains(t, body, "Page 2")
}

func TestStorePartialAndHistoryRestore(t *testing.T) {
	h, _ := newTestUI(t, withProducts(shoes()))

	partial := get(h, "/?q=run", "HX-Request", "true")
	require.Equal(t, http.StatusOK, partial.Code)
	require.NotContains(t, partial.Body.String(), "<html")
	require.NotContains(t, partial.Body.String(), `<form class="filters"`)
	require.Contains(t, partial.Body.String(), "Running Shoes")

	restore := get(h, "/?q=run", "HX-Request", "true", "HX-History-Restore-Request", "true")
	require.Contains(t, restore.Body.String(), "<html")
}

func TestStoreShowsFilterErrorsNextToTheField(t *testing.T) {
	cat := withProducts(shoes())
	h, _ := newTestUI(t, cat)

	rec := get(h, "/?min_price=cheap&max_price=10")

	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, cat.searched)
	body := rec.Body.String()
	require.Contains(t, fieldBlock(body, "min_price"), `<p id="error-min_price" class="field-error">must be a plain decimal number such as 29.99</p>`)
	require.Contains(t, fieldBlock(body, "min_price"), `value="cheap"`)
	require.Contains(t, fieldBlock(body, "max_price"), `<p id="error-max_price" class="field-error"></p>`)
	require.Contains(t, body, "Some filters are not valid")
	require.NotContains(t, body, "hx-swap-oob", "a full page has no out-of-band updates")
}

func TestStoreRejectsExponentFilters(t *testing.T) {
	cat := withProducts(shoes())
	h, _ := newTestUI(t, cat)

	rec := get(h, "/?min_price=1e100000000")

	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, cat.searched)
	require.Contains(t, fieldBlock(rec.Body.String(), "min_price"), `<p id="error-min_price" class="field-error">must be a plain decimal number such as 29.99</p>`)
}

func TestStorePartialUpdatesFilterErrorsOutOfBand(t *testing.T) {
	h, _ := newTestUI(t, withProducts(shoes()))

	invalid := get(h, "/?min_price=cheap", "HX-Request", "true").Body.String()
	require.NotContains(t, invalid, "<form")
	require.Contains(t, invalid, `<p id="error-min_price" class="field-error" hx-swap-oob="true">must be a plain decimal number such as 29.99</p>`)
	require.Contains(t, invalid, `<p id="error-q" class="field-error" hx-swap-oob="true"></p>`)

	fixed := get(h, "/?min_price=10", "HX-Request", "true").Body.String()
	require.Contains(t, fixed, `<p id="error-min_price" class="field-error" hx-swap-oob="true"></p>`, "fixing a filter clears its error")
	require.Contains(t, fixed, "Running Shoes")

	admin := get(h, "/admin/products?q="+strings.Repeat("x", 101), "HX-Request", "true").Body.String()
	require.Contains(t, admin, `<p id="error-q" class="field-error" hx-swap-oob="true">must be at most 100 characters</p>`)
}

func TestPagesHaveTheirOwnTitle(t *testing.T) {
	h, _ := newTestUI(t, withProducts(shoes()))

	tests := map[string]string{
		"/":                                      "<title>Store · Shops</title>",
		"/products/" + productID:                 "<title>Running Shoes · Shops</title>",
		"/admin/products":                        "<title>Admin · Products · Shops</title>",
		"/admin/products/new":                    "<title>New product · Shops</title>",
		"/admin/products/" + productID + "/edit": "<title>Edit product · Shops</title>",
		"/no-existe":                             "<title>Not found · Shops</title>",
	}
	for path, want := range tests {
		require.Contains(t, get(h, path).Body.String(), want, path)
	}
}

func TestProductDetailNotFoundIsHTML(t *testing.T) {
	h, _ := newTestUI(t, withProducts())

	rec := get(h, "/products/"+productID)

	require.Equal(t, http.StatusNotFound, rec.Code)
	requireSafeHTML(t, rec)
	require.Contains(t, rec.Body.String(), "Not found")
}

func TestCatalogFailuresRenderAnHTMLErrorPage(t *testing.T) {
	cat := withProducts(shoes())
	cat.err = errors.New("db exploded: host=10.0.0.5")
	h, logs := newTestUI(t, cat)

	for _, path := range []string{"/", "/products/" + productID} {
		rec := get(h, path)

		require.Equal(t, http.StatusInternalServerError, rec.Code, path)
		requireSafeHTML(t, rec)
		require.NotContains(t, rec.Body.String(), "10.0.0.5")
	}
	require.Contains(t, logs.String(), "db exploded")
}
