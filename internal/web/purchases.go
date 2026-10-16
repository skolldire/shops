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
	Error   string
}

func (ui *UI) purchases(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
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
