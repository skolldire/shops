package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/ordering"
)

type purchasesPage struct {
	Page    ordering.PurchasePage
	PrevURL string
	NextURL string
}

func (ui *UI) purchases(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 || page > 100000 {
		page = 1
	}
	res, err := ui.orders.Purchases(r.Context(), ui.currentUser(r), page, 20)
	if err != nil {
		ui.serverError(w, r, err)
		return
	}
	data := purchasesPage{Page: res}
	if res.Page > 1 {
		data.PrevURL = "/purchases?page=" + strconv.Itoa(res.Page-1)
	}
	if res.Page*res.PageSize < res.Total {
		data.NextURL = "/purchases?page=" + strconv.Itoa(res.Page+1)
	}
	ui.page(w, r, http.StatusOK, "purchases", data)
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
