package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/validation"
)

func post(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func validForm() url.Values {
	return url.Values{
		"sku": {"rs-001"}, "name": {"Running Shoes"}, "description": {"Light"}, "category": {"Sports"},
		"price": {"29.99"}, "stock": {"10"}, "weight_kg": {"0.850"},
	}
}

func fieldBlock(body, field string) string {
	start := strings.Index(body, `<label for="`+field+`">`)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</div>")
	return body[start : start+end]
}

func TestAdminListShowsActions(t *testing.T) {
	h, _ := newTestUI(t, withProducts(shoes()))

	rec := get(h, "/admin/products?notice=updated")

	require.Equal(t, http.StatusOK, rec.Code)
	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.Contains(t, body, "Product updated.")
	require.Contains(t, body, `<a href="/admin/products/`+productID+`/edit">Edit</a>`)
	require.Contains(t, body, `<a href="/admin/products/`+productID+`/delete">Delete</a>`)
	require.Contains(t, body, `<a class="button" href="/admin/products/new">New product</a>`)

	partial := get(h, "/admin/products?q=run", "HX-Request", "true")
	require.NotContains(t, partial.Body.String(), "<html")
	require.Contains(t, partial.Body.String(), "<table>")
}

func TestAdminListDoesNotDependOnCategories(t *testing.T) {
	cat := withProducts(shoes())
	cat.catErr = errors.New("categories unavailable")
	h, _ := newTestUI(t, cat)

	rec := get(h, "/admin/products")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Running Shoes")
	require.Equal(t, http.StatusInternalServerError, get(h, "/").Code, "the storefront still needs its category filter")
}

func TestCreateProduct(t *testing.T) {
	cat := withProducts()
	h, _ := newTestUI(t, cat)

	form := get(h, "/admin/products/new")
	require.Equal(t, http.StatusOK, form.Code)
	requireSafeHTML(t, form)

	rec := post(h, "/admin/products", validForm())

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/admin/products?notice=created", rec.Header().Get("Location"))
	require.Equal(t, "rs-001", cat.created.SKU)
	require.Equal(t, "29.99", cat.created.Price.String())
	require.Equal(t, "0.85", cat.created.WeightKg.String())
	require.EqualValues(t, 10, *cat.created.Stock)
}

func TestCreateProductFormatErrorsStayNextToTheirFields(t *testing.T) {
	cat := withProducts()
	h, _ := newTestUI(t, cat)
	form := validForm()
	form.Set("price", "cheap")
	form.Set("stock", "2147483648")
	form.Set("weight_kg", "1,5")

	rec := post(h, "/admin/products", form)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.Contains(t, fieldBlock(body, "price"), "must be a plain decimal number such as 29.99")
	require.Contains(t, fieldBlock(body, "price"), `value="cheap"`)
	require.Contains(t, fieldBlock(body, "stock"), "must be between 0 and 2147483647")
	require.Contains(t, fieldBlock(body, "weight_kg"), "must be a plain decimal number such as 29.99")
	require.NotContains(t, fieldBlock(body, "name"), "field-error")
	require.Empty(t, cat.created.SKU, "invalid forms must not reach the catalog")
}

func TestCreateProductRejectsExponents(t *testing.T) {
	cat := withProducts()
	h, _ := newTestUI(t, cat)
	form := validForm()
	form.Set("price", "1e100000000")

	rec := post(h, "/admin/products", form)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Contains(t, fieldBlock(rec.Body.String(), "price"), "must be a plain decimal number such as 29.99")
	require.Empty(t, cat.created.SKU)
}

func TestCreateProductDomainErrors(t *testing.T) {
	var errs validation.Errors
	errs.Add("name", validation.CodeRequired, "is required")
	errs.Add("price", validation.CodeNotPositive, "must be greater than 0")
	cat := withProducts()
	cat.writeErr = errs.Err()
	h, _ := newTestUI(t, cat)

	rec := post(h, "/admin/products", validForm())

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	body := rec.Body.String()
	require.Contains(t, fieldBlock(body, "name"), "is required")
	require.Contains(t, fieldBlock(body, "price"), "must be greater than 0")
	require.Contains(t, body, `value="rs-001"`)
}

func TestCreateProductSKUTaken(t *testing.T) {
	cat := withProducts()
	cat.writeErr = catalog.ErrSKUTaken
	h, _ := newTestUI(t, cat)

	rec := post(h, "/admin/products", validForm())

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Contains(t, fieldBlock(rec.Body.String(), "sku"), "is already used by another product")
}

func TestEditProduct(t *testing.T) {
	cat := withProducts(shoes())
	h, _ := newTestUI(t, cat)

	form := get(h, "/admin/products/"+productID+"/edit")
	require.Equal(t, http.StatusOK, form.Code)
	requireSafeHTML(t, form)
	body := form.Body.String()
	require.Contains(t, body, `<input type="hidden" name="version" value="2">`)
	require.Contains(t, body, `value="RS-001" maxlength="64" readonly`)
	require.Contains(t, body, `value="29.90"`)
	require.Contains(t, body, `value="0.850"`)

	values := validForm()
	values.Set("version", "2")
	values.Set("name", "Trail Shoes")
	rec := post(h, "/admin/products/"+productID, values)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/admin/products?notice=updated", rec.Header().Get("Location"))
	require.Equal(t, 2, cat.updated.Version)
	require.Equal(t, "Trail Shoes", *cat.updated.Name)
	require.Nil(t, cat.updated.SKU, "the SKU is never sent on update")
	require.EqualValues(t, 10, *cat.updated.Stock)
}

func TestEditProductRequiresNumbers(t *testing.T) {
	cat := withProducts(shoes())
	h, _ := newTestUI(t, cat)
	values := validForm()
	values.Set("version", "2")
	values.Set("price", "  ")
	values.Set("stock", "")
	values.Set("weight_kg", "")

	rec := post(h, "/admin/products/"+productID, values)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	body := rec.Body.String()
	for _, field := range []string{"price", "stock", "weight_kg"} {
		require.Contains(t, fieldBlock(body, field), "is required", field)
	}
	require.Zero(t, cat.updated.Version, "a blank field must not be silently skipped")
}

func TestEditProductVersionConflict(t *testing.T) {
	cat := withProducts(shoes())
	cat.writeErr = catalog.ErrVersionConflict
	h, _ := newTestUI(t, cat)
	values := validForm()
	values.Set("version", "1")

	rec := post(h, "/admin/products/"+productID, values)

	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.Contains(t, body, "changed by someone else")
	require.Contains(t, body, `href="/admin/products/`+productID+`/edit">Reload the latest version</a>`)
	require.Contains(t, body, `value="Running Shoes"`)
}

func TestEditUnknownProductIsNotFound(t *testing.T) {
	cat := withProducts()
	h, _ := newTestUI(t, cat)

	require.Equal(t, http.StatusNotFound, get(h, "/admin/products/"+productID+"/edit").Code)
	require.Equal(t, http.StatusNotFound, get(h, "/admin/products/"+productID+"/delete").Code)

	cat.writeErr = catalog.ErrNotFound
	values := validForm()
	values.Set("version", "1")
	require.Equal(t, http.StatusNotFound, post(h, "/admin/products/"+productID, values).Code)
}

func TestDeleteProduct(t *testing.T) {
	cat := withProducts(shoes())
	h, _ := newTestUI(t, cat)

	confirm := get(h, "/admin/products/"+productID+"/delete")
	require.Equal(t, http.StatusOK, confirm.Code)
	requireSafeHTML(t, confirm)
	body := confirm.Body.String()
	require.Contains(t, body, `<form action="/admin/products/`+productID+`/delete" method="post"`)
	require.Contains(t, body, `<input type="hidden" name="version" value="2">`)
	require.NotContains(t, body, "confirm(")

	rec := post(h, "/admin/products/"+productID+"/delete", url.Values{"version": {"2"}})

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/admin/products?notice=deleted", rec.Header().Get("Location"))
	require.Equal(t, 2, cat.deleted)
}

func TestDeleteProductVersionConflict(t *testing.T) {
	cat := withProducts(shoes())
	cat.writeErr = catalog.ErrVersionConflict
	h, _ := newTestUI(t, cat)

	rec := post(h, "/admin/products/"+productID+"/delete", url.Values{"version": {"1"}})

	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "changed after you opened this page")
	require.Contains(t, body, `<input type="hidden" name="version" value="2">`)
}

func TestAdminEscapesUserContent(t *testing.T) {
	evil := shoes()
	evil.Name = "<script>alert(1)</script>"
	h, _ := newTestUI(t, withProducts(evil))

	for _, path := range []string{"/admin/products", "/admin/products/" + productID + "/edit", "/admin/products/" + productID + "/delete"} {
		rec := get(h, path)

		require.NotContains(t, rec.Body.String(), "<script>alert(1)</script>", path)
		requireSafeHTML(t, rec)
	}
}

func TestOversizedFormIsRejected(t *testing.T) {
	h, _ := newTestUI(t, withProducts())
	form := validForm()
	form.Set("description", strings.Repeat("x", maxFormBytes))

	rec := post(h, "/admin/products", form)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	requireSafeHTML(t, rec)
}
