package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/validation"
)

const adminProducts = "/admin/products"

type formValues struct {
	SKU, Name, Description, Category, Price, Stock, WeightKg string
}

type productForm struct {
	Title    string
	Action   string
	Submit   string
	Editing  bool
	ID       string
	Version  int
	Values   formValues
	Errors   map[string]string
	Conflict bool
}

type deletePage struct {
	Product  catalog.Product
	Conflict bool
}

func (ui *UI) adminList(w http.ResponseWriter, r *http.Request) {
	data, err := ui.listing(r, adminProducts)
	if err != nil {
		ui.serverError(w, r, err)
		return
	}
	ui.listingPage(w, r, "admin_list", data)
}

func (ui *UI) newProduct(w http.ResponseWriter, r *http.Request) {
	ui.page(w, r, http.StatusOK, "admin_form", newProductForm(formValues{}, nil))
}

func (ui *UI) createProduct(w http.ResponseWriter, r *http.Request) {
	values, err := readForm(r)
	if err != nil {
		ui.badForm(w, r, err)
		return
	}
	price, stock, weight, formatErrs := parseNumbers(values)
	if len(formatErrs) > 0 {
		ui.page(w, r, http.StatusUnprocessableEntity, "admin_form", newProductForm(values, formatErrs))
		return
	}
	_, err = ui.catalog.Create(r.Context(), catalog.CreateInput{
		SKU: values.SKU, Name: values.Name, Description: values.Description, Category: values.Category,
		Price: price, Stock: stock, WeightKg: weight,
	})
	if errors.Is(err, catalog.ErrSKUTaken) {
		err = fieldError("sku", "is already used by another product")
	}
	if fieldErrs, ok := asFieldErrors(err); ok {
		ui.page(w, r, http.StatusUnprocessableEntity, "admin_form", newProductForm(values, fieldErrs))
		return
	}
	if err != nil {
		ui.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, adminProducts+"?notice=created", http.StatusSeeOther)
}

func (ui *UI) editProduct(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.loadProduct(w, r)
	if !ok {
		return
	}
	ui.page(w, r, http.StatusOK, "admin_form", editProductForm(p.ID, p.Version, valuesOf(p), nil, false))
}

func (ui *UI) updateProduct(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	values, err := readForm(r)
	if err != nil {
		ui.badForm(w, r, err)
		return
	}
	version := formVersion(r)
	price, stock, weight, formatErrs := parseNumbers(values)
	if len(formatErrs) > 0 {
		ui.page(w, r, http.StatusUnprocessableEntity, "admin_form", editProductForm(id, version, values, formatErrs, false))
		return
	}
	_, err = ui.catalog.Update(r.Context(), id, catalog.UpdateInput{
		Version: version, Name: &values.Name, Description: &values.Description, Category: &values.Category,
		Price: price, Stock: stock, WeightKg: weight,
	})
	if fieldErrs, ok := asFieldErrors(err); ok {
		ui.page(w, r, http.StatusUnprocessableEntity, "admin_form", editProductForm(id, version, values, fieldErrs, false))
		return
	}
	switch {
	case errors.Is(err, catalog.ErrVersionConflict):
		ui.page(w, r, http.StatusPreconditionFailed, "admin_form", editProductForm(id, version, values, nil, true))
	case errors.Is(err, catalog.ErrNotFound):
		ui.notFound(w, r)
	case err != nil:
		ui.serverError(w, r, err)
	default:
		http.Redirect(w, r, adminProducts+"?notice=updated", http.StatusSeeOther)
	}
}

func (ui *UI) confirmDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := ui.loadProduct(w, r)
	if !ok {
		return
	}
	ui.page(w, r, http.StatusOK, "admin_delete", deletePage{Product: p})
}

func (ui *UI) deleteProduct(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		ui.badForm(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	err := ui.catalog.Delete(r.Context(), id, formVersion(r))
	switch {
	case errors.Is(err, catalog.ErrVersionConflict):
		p, ok := ui.loadProduct(w, r)
		if ok {
			ui.page(w, r, http.StatusPreconditionFailed, "admin_delete", deletePage{Product: p, Conflict: true})
		}
	case errors.Is(err, catalog.ErrNotFound):
		ui.notFound(w, r)
	case err != nil:
		ui.serverError(w, r, err)
	default:
		http.Redirect(w, r, adminProducts+"?notice=deleted", http.StatusSeeOther)
	}
}

func (ui *UI) loadProduct(w http.ResponseWriter, r *http.Request) (catalog.Product, bool) {
	p, err := ui.catalog.Get(r.Context(), chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		ui.notFound(w, r)
		return catalog.Product{}, false
	case err != nil:
		ui.serverError(w, r, err)
		return catalog.Product{}, false
	}
	return p, true
}

func (ui *UI) badForm(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	status, message := http.StatusBadRequest, "The form could not be read."
	if errors.As(err, &tooLarge) {
		status, message = http.StatusRequestEntityTooLarge, "The form is too large."
	}
	ui.page(w, r, status, "error", errorPage{Title: "Invalid form", Status: status, Message: message})
}

func newProductForm(values formValues, errs map[string]string) productForm {
	return productForm{Title: "New product", Action: adminProducts, Submit: "Create product", Values: values, Errors: errs}
}

func editProductForm(id string, version int, values formValues, errs map[string]string, conflict bool) productForm {
	return productForm{
		Title: "Edit product", Action: adminProducts + "/" + id, Submit: "Save changes", Editing: true,
		ID: id, Version: version, Values: values, Errors: errs, Conflict: conflict,
	}
}

func readForm(r *http.Request) (formValues, error) {
	if err := r.ParseForm(); err != nil {
		return formValues{}, err
	}
	return formValues{
		SKU: r.PostForm.Get("sku"), Name: r.PostForm.Get("name"), Description: r.PostForm.Get("description"),
		Category: r.PostForm.Get("category"), Price: r.PostForm.Get("price"), Stock: r.PostForm.Get("stock"),
		WeightKg: r.PostForm.Get("weight_kg"),
	}, nil
}

func formVersion(r *http.Request) int {
	version, err := strconv.Atoi(r.PostForm.Get("version"))
	if err != nil {
		return 0
	}
	return version
}

func parseNumbers(v formValues) (price *decimal.Decimal, stock *int64, weight *decimal.Decimal, errs map[string]string) {
	errs = map[string]string{}
	price = parseFormDecimal(errs, "price", v.Price)
	weight = parseFormDecimal(errs, "weight_kg", v.WeightKg)
	if raw := strings.TrimSpace(v.Stock); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		var numErr *strconv.NumError
		switch {
		case errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange):
			errs["stock"] = "must be between 0 and 2147483647"
		case err != nil:
			errs["stock"] = "must be a whole number"
		default:
			stock = &n
		}
	}
	return price, stock, weight, errs
}

func parseFormDecimal(errs map[string]string, field, raw string) *decimal.Decimal {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		errs[field] = "must be a decimal number"
		return nil
	}
	return &d
}

func asFieldErrors(err error) (map[string]string, bool) {
	var fieldErrs validation.Errors
	if !errors.As(err, &fieldErrs) {
		return nil, false
	}
	out := make(map[string]string, len(fieldErrs))
	for _, fe := range fieldErrs {
		out[fe.Field] = fe.Message
	}
	return out, true
}

func fieldError(field, message string) error {
	var errs validation.Errors
	errs.Add(field, validation.CodeInvalidFormat, message)
	return errs.Err()
}

func valuesOf(p catalog.Product) formValues {
	return formValues{
		SKU: p.SKU, Name: p.Name, Description: p.Description, Category: p.Category,
		Price: p.Price.StringFixed(2), Stock: strconv.Itoa(p.Stock), WeightKg: p.WeightKg.StringFixed(3),
	}
}
