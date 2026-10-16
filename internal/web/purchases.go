package web

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
)

type purchasesPage struct {
	Page    ordering.PurchasePage
	PrevURL string
	NextURL string
	Error   string
}

func (ui *UI) purchases(w http.ResponseWriter, r *http.Request) {
	page, pageOK := pagingParam(r.URL.Query().Get("page"))
	size, sizeOK := pagingParam(r.URL.Query().Get("page_size"))
	if !pageOK || !sizeOK {
		ui.page(w, r, http.StatusUnprocessableEntity, "purchases", purchasesPage{Error: "The purchase page or page size is invalid."})
		return
	}
	res, err := ui.orders.Purchases(r.Context(), ui.currentUser(r), page, size)
	if isFieldError(err) {
		ui.page(w, r, http.StatusUnprocessableEntity, "purchases", purchasesPage{Error: "The purchase page or page size is invalid."})
		return
	}
	if err != nil {
		ui.serverError(w, r, err)
		return
	}
	data := purchasesPage{Page: res}
	if res.Page > 1 {
		data.PrevURL = movementsURL("/purchases", res.Page-1, res.PageSize)
	}
	if res.Page*res.PageSize < res.Total {
		data.NextURL = movementsURL("/purchases", res.Page+1, res.PageSize)
	}
	ui.page(w, r, http.StatusOK, "purchases", data)
}

func pagingParam(raw string) (*int, bool) {
	if raw == "" {
		return nil, true
	}
	n, err := catalog.ParseWholeNumber(raw)
	if err != nil {
		return nil, false
	}
	value := int(n)
	return &value, true
}

func (ui *UI) purchase(w http.ResponseWriter, r *http.Request) {
	p, err := ui.orders.Purchase(r.Context(), ui.currentUser(r), chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, ordering.ErrPurchaseNotFound):
		ui.notFound(w, r)
	case err != nil:
		ui.serverError(w, r, err)
	default:
		ui.page(w, r, http.StatusOK, "purchase", p)
	}
}
