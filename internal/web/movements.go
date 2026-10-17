package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog"
)

type movementsPage struct {
	Product catalog.Product
	Page    catalog.MovementPage
	Invalid bool
	PrevURL string
	NextURL string
}

func (ui *UI) movements(w http.ResponseWriter, r *http.Request) {
	p, err := ui.catalog.Get(r.Context(), chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		ui.notFound(w, r)
		return
	case err != nil:
		ui.serverError(w, r, err)
		return
	}
	data := movementsPage{Product: p}
	paging, err := catalog.ParsePaging(r.URL.Query().Get("page"), r.URL.Query().Get("page_size"))
	if _, invalid := asFieldErrors(err); invalid {
		data.Invalid = true
		ui.page(w, r, http.StatusOK, "admin_movements", data)
		return
	}
	if err != nil {
		ui.serverError(w, r, err)
		return
	}
	data.Page, err = ui.catalog.Movements(r.Context(), p.ID, paging)
	if err != nil {
		ui.serverError(w, r, err)
		return
	}
	path := adminProducts + "/" + p.ID + "/movements"
	if data.Page.Page > 1 {
		data.PrevURL = pagingURL(path, data.Page.Page-1, data.Page.PageSize)
	}
	if data.Page.Page*data.Page.PageSize < data.Page.Total {
		data.NextURL = pagingURL(path, data.Page.Page+1, data.Page.PageSize)
	}
	ui.page(w, r, http.StatusOK, "admin_movements", data)
}

func pagingURL(path string, page, pageSize int) string {
	return path + "?" + url.Values{"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)}}.Encode()
}
