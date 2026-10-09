package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/validation"
)

type sortOption struct {
	Value, Label string
}

var sortOptions = []sortOption{
	{"name", "Name (A to Z)"},
	{"-name", "Name (Z to A)"},
	{"price", "Price (low to high)"},
	{"-price", "Price (high to low)"},
	{"-created_at", "Newest first"},
	{"created_at", "Oldest first"},
}

type filters struct {
	Q, Category, MinPrice, MaxPrice, Sort string
	InStock                               bool
}

type listing struct {
	Path        string
	Filters     filters
	Errors      map[string]string
	Categories  []string
	SortOptions []sortOption
	Page        catalog.Page
	First, Last int
	PrevURL     string
	NextURL     string
	Notice      string
}

func (ui *UI) store(w http.ResponseWriter, r *http.Request) {
	data, err := ui.listing(r, "/")
	if err != nil {
		ui.serverError(w, r, err)
		return
	}
	if data.Categories, err = ui.catalog.Categories(r.Context()); err != nil {
		ui.serverError(w, r, err)
		return
	}
	ui.listingPage(w, r, "store", data)
}

func (ui *UI) product(w http.ResponseWriter, r *http.Request) {
	p, err := ui.catalog.Get(r.Context(), chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		ui.notFound(w, r)
	case err != nil:
		ui.serverError(w, r, err)
	default:
		ui.page(w, r, http.StatusOK, "product", p)
	}
}

func (ui *UI) listing(r *http.Request, path string) (listing, error) {
	values := r.URL.Query()
	inStock, _ := strconv.ParseBool(values.Get("in_stock"))
	data := listing{
		Path: path,
		Filters: filters{
			Q: values.Get("q"), Category: values.Get("category"), MinPrice: values.Get("min_price"),
			MaxPrice: values.Get("max_price"), Sort: values.Get("sort"), InStock: inStock,
		},
		SortOptions: sortOptions,
		Notice:      notices[values.Get("notice")],
	}
	query, err := catalog.ParseSearchQuery(values)
	var fieldErrs validation.Errors
	if errors.As(err, &fieldErrs) {
		data.Errors = make(map[string]string, len(fieldErrs))
		for _, fe := range fieldErrs {
			data.Errors[fe.Field] = fe.Message
		}
		return data, nil
	}
	if err != nil {
		return listing{}, err
	}

	page, err := ui.catalog.Search(r.Context(), query)
	if err != nil {
		return listing{}, err
	}
	data.Page = page
	if len(page.Items) > 0 {
		data.First = (page.Page-1)*page.PageSize + 1
		data.Last = data.First + len(page.Items) - 1
	}
	if page.Page > 1 {
		data.PrevURL = pageURL(path, values, page.Page-1)
	}
	if page.Page*page.PageSize < page.Total {
		data.NextURL = pageURL(path, values, page.Page+1)
	}
	return data, nil
}

func (ui *UI) listingPage(w http.ResponseWriter, r *http.Request, page string, data listing) {
	if isPartial(r) {
		if err := ui.views.render(w, http.StatusOK, page, "partial", data); err != nil {
			ui.serverError(w, r, err)
		}
		return
	}
	ui.page(w, r, http.StatusOK, page, data)
}

func isPartial(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true"
}

func pageURL(path string, values url.Values, page int) string {
	next := url.Values{}
	for key, v := range values {
		if key != "page" && key != "notice" {
			next[key] = v
		}
	}
	next.Set("page", strconv.Itoa(page))
	return path + "?" + next.Encode()
}
