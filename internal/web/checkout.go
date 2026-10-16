package web

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/skolldire/shops/internal/ordering"
)

type tokenOption struct {
	Value, Label string
}

var paymentTokens = []tokenOption{
	{"pm_approved", "Test card that is approved (pm_approved)"},
	{"pm_declined", "Test card that is declined (pm_declined)"},
	{"pm_unknown", "Test card whose answer is lost, so the purchase stays pending until the recovery confirms it (pm_unknown)"},
}

type checkoutPage struct {
	Cart   ordering.Cart
	Key    string
	Token  string
	Tokens []tokenOption
	Notice string
}

func (ui *UI) checkoutForm(w http.ResponseWriter, r *http.Request) {
	ui.renderCheckout(w, r, http.StatusOK, uuid.NewString(), paymentTokens[0].Value, "")
}

func (ui *UI) renderCheckout(w http.ResponseWriter, r *http.Request, status int, key, token, notice string) {
	cart, err := ui.orders.Cart(r.Context(), ui.currentUser(r))
	if err != nil {
		ui.serverError(w, r, err)
		return
	}
	if len(cart.Lines) == 0 {
		http.Redirect(w, r, "/cart?notice=empty", http.StatusSeeOther)
		return
	}
	if cart.HasWarnings {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}
	ui.page(w, r, status, "checkout", checkoutPage{Cart: cart, Key: key, Token: token, Tokens: paymentTokens, Notice: notice})
}

func (ui *UI) checkout(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		ui.badForm(w, r, err)
		return
	}
	key, token := r.PostForm.Get("idempotency_key"), r.PostForm.Get("payment_token")
	res, err := ui.orders.CheckoutCart(r.Context(), ui.currentUser(r), key, token)
	var rejected *ordering.RejectedError
	switch {
	case err == nil:
		http.Redirect(w, r, "/purchases/"+res.Purchase.ID, http.StatusSeeOther)
	case errors.As(err, &rejected) && errors.Is(err, ordering.ErrPriceChanged):
		http.Redirect(w, r, "/cart?notice=price_changed", http.StatusSeeOther)
	case errors.As(err, &rejected) && errors.Is(err, ordering.ErrInsufficientStock):
		http.Redirect(w, r, "/cart?notice=stock", http.StatusSeeOther)
	case errors.As(err, &rejected):
		http.Redirect(w, r, "/cart?notice=unavailable", http.StatusSeeOther)
	case errors.Is(err, ordering.ErrIdempotencyConflict):
		ui.renderCheckout(w, r, http.StatusConflict, uuid.NewString(), token,
			"Your cart changed since this page was opened. Review the order and confirm it again.")
	case errors.Is(err, ordering.ErrInvalidIdempotencyKey):
		ui.renderCheckout(w, r, http.StatusBadRequest, uuid.NewString(), token,
			"The order form was not valid. Review the order and confirm it again.")
	case errors.Is(err, ordering.ErrTryAgain):
		w.Header().Set("Retry-After", "1")
		ui.renderCheckout(w, r, http.StatusServiceUnavailable, key, token,
			"The store was busy and nothing was charged. Confirm again in a moment.")
	case isFieldError(err):
		http.Redirect(w, r, "/cart?notice=empty", http.StatusSeeOther)
	default:
		ui.serverError(w, r, err)
	}
}
