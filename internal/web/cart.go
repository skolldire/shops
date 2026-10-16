package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
)

var cartNotices = map[string]string{
	"added":          "The product was added to your cart.",
	"updated":        "The quantity was updated.",
	"removed":        "The product was removed from your cart.",
	"price_accepted": "The line now uses the current price.",
	"price_changed":  "Some prices changed since you added the products. Review them and use the current price before paying.",
	"stock":          "There is not enough stock for some products. Lower the quantity before paying.",
	"unavailable":    "Some products are no longer available. Remove them before paying.",
	"quantity":       "The quantity must be a whole number between 1 and 99.",
	"empty":          "Your cart is empty.",
	"missing":        "That product is not available.",
}

type productPage struct {
	Product catalog.Product
	Error   string
}

type cartPage struct {
	Cart   ordering.Cart
	Notice string
	Alert  bool
}

var cartFullMessage = fmt.Sprintf("Your cart already has %d different products. Remove one before adding another.", ordering.MaxCartLines)

func (ui *UI) cart(w http.ResponseWriter, r *http.Request) {
	cart, err := ui.orders.Cart(r.Context(), ui.currentUser(r))
	if err != nil {
		ui.serverError(w, r, err)
		return
	}
	notice := r.URL.Query().Get("notice")
	alert := notice == "price_changed" || notice == "stock" || notice == "unavailable" || notice == "quantity" || notice == "missing"
	ui.page(w, r, http.StatusOK, "cart", cartPage{Cart: cart, Notice: cartNotices[notice], Alert: alert})
}

func (ui *UI) addToCart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		ui.badForm(w, r, err)
		return
	}
	id := r.PostForm.Get("product_id")
	quantity, ok := formQuantity(r.PostForm.Get("quantity"))
	var err error
	if ok {
		err = ui.orders.AddToCart(r.Context(), ui.currentUser(r), id, quantity)
	}
	switch {
	case ok && err == nil:
		http.Redirect(w, r, "/cart?notice=added", http.StatusSeeOther)
		return
	case errors.Is(err, ordering.ErrProductUnavailable), errors.Is(err, ordering.ErrInvalidID):
		http.Redirect(w, r, "/cart?notice=missing", http.StatusSeeOther)
		return
	case err != nil && !errors.Is(err, ordering.ErrCartFull) && !isFieldError(err):
		ui.serverError(w, r, err)
		return
	}
	p, getErr := ui.catalog.Get(r.Context(), id)
	switch {
	case errors.Is(getErr, catalog.ErrNotFound):
		ui.notFound(w, r)
		return
	case getErr != nil:
		ui.serverError(w, r, getErr)
		return
	}
	message := cartNotices["quantity"]
	if fields, ok := asFieldErrors(err); ok && fields["quantity"] != "" {
		message = fields["quantity"]
	}
	if errors.Is(err, ordering.ErrCartFull) {
		message = cartFullMessage
	}
	ui.page(w, r, http.StatusUnprocessableEntity, "product", productPage{Product: p, Error: message})
}

func (ui *UI) setQuantity(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		ui.badForm(w, r, err)
		return
	}
	quantity, ok := formQuantity(r.PostForm.Get("quantity"))
	if !ok {
		http.Redirect(w, r, "/cart?notice=quantity", http.StatusSeeOther)
		return
	}
	ui.cartChange(w, r, "updated", ui.orders.SetQuantity(r.Context(), ui.currentUser(r), chi.URLParam(r, "id"), quantity))
}

func (ui *UI) removeFromCart(w http.ResponseWriter, r *http.Request) {
	ui.cartChange(w, r, "removed", ui.orders.RemoveFromCart(r.Context(), ui.currentUser(r), chi.URLParam(r, "id")))
}

func (ui *UI) acceptPrice(w http.ResponseWriter, r *http.Request) {
	ui.cartChange(w, r, "price_accepted", ui.orders.AcceptCurrentPrice(r.Context(), ui.currentUser(r), chi.URLParam(r, "id")))
}

func (ui *UI) cartChange(w http.ResponseWriter, r *http.Request, notice string, err error) {
	switch {
	case err == nil:
	case errors.Is(err, ordering.ErrProductUnavailable), errors.Is(err, ordering.ErrInvalidID):
		notice = "missing"
	case isFieldError(err):
		notice = "quantity"
	default:
		ui.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/cart?notice="+notice, http.StatusSeeOther)
}

func formQuantity(raw string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return n, true
}

func isFieldError(err error) bool {
	_, ok := asFieldErrors(err)
	return ok
}
