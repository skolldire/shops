package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/validation"
)

const IdempotencyKeyHeader = "Idempotency-Key"

type Checkout interface {
	Checkout(ctx context.Context, userID string, in domain.CheckoutInput) (app.CheckoutResult, error)
}

type Queries interface {
	Purchase(ctx context.Context, userID, id string) (domain.Purchase, error)
	Purchases(ctx context.Context, userID string, page, pageSize *int) (app.PurchasePage, error)
}

type Handler struct {
	checkout Checkout
	queries  Queries
	users    map[string]bool
	log      logger.Service
}

func New(checkout Checkout, queries Queries, users []string, log logger.Service) (*Handler, error) {
	switch {
	case checkout == nil, queries == nil:
		return nil, errors.New("ordering: http handler requires the checkout and the queries")
	case len(users) == 0:
		return nil, errors.New("ordering: http handler requires the demo users")
	case log == nil:
		return nil, errors.New("ordering: http handler requires a logger")
	}
	set := make(map[string]bool, len(users))
	for _, u := range users {
		set[u] = true
	}
	return &Handler{checkout: checkout, queries: queries, users: set, log: log}, nil
}

func (h *Handler) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.demoUser)
		r.Post("/checkout", h.create)
		r.Get("/purchases", h.list)
		r.Get("/purchases/{id}", h.get)
	})
}

type itemJSON struct {
	ProductID string `json:"product_id"`
	SKU       string `json:"sku"`
	Name      string `json:"name"`
	UnitPrice string `json:"unit_price"`
	Quantity  int    `json:"quantity"`
	LineTotal string `json:"line_total"`
}

type purchaseJSON struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	Currency      string     `json:"currency"`
	Total         string     `json:"total"`
	PaymentRef    *string    `json:"payment_ref"`
	FailureReason *string    `json:"failure_reason"`
	CreatedAt     time.Time  `json:"created_at"`
	Items         []itemJSON `json:"items"`
}

type pageJSON struct {
	Items    []purchaseJSON `json:"items"`
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
	Total    int            `json:"total"`
}

func toJSON(p domain.Purchase) purchaseJSON {
	out := purchaseJSON{
		ID: string(p.ID), Status: string(p.Status), Currency: p.Currency, Total: p.Total.StringFixed(2),
		CreatedAt: p.CreatedAt.UTC(), Items: make([]itemJSON, len(p.Items)),
	}
	if p.PaymentRef != "" {
		out.PaymentRef = &p.PaymentRef
	}
	if p.FailureReason != "" {
		out.FailureReason = &p.FailureReason
	}
	for i, it := range p.Items {
		out.Items[i] = itemJSON{
			ProductID: string(it.ProductID), SKU: it.SKU, Name: it.Name, UnitPrice: it.UnitPrice.StringFixed(2),
			Quantity: it.Quantity, LineTotal: it.LineTotal.StringFixed(2),
		}
	}
	return out
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	body, err := decodeCheckout(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	in, format := body.input(r.Header.Get(IdempotencyKeyHeader))
	if len(format) > 0 {
		h.writeError(w, r, format)
		return
	}
	res, err := h.checkout.Checkout(r.Context(), userFrom(r.Context()), in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	switch {
	case res.Kind == app.InProgress, res.Purchase.Status == domain.StatusPending:
		status = http.StatusAccepted
	case res.Kind == app.Created:
		status = http.StatusCreated
	}
	if status != http.StatusOK {
		w.Header().Set("Location", "/api/v1/purchases/"+string(res.Purchase.ID))
	}
	httpx.WriteJSON(w, status, toJSON(res.Purchase))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, err := h.queries.Purchase(r.Context(), userFrom(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toJSON(p))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	var format validation.Errors
	page := pageParam(&format, "page", r.URL.Query().Get("page"))
	size := pageParam(&format, "page_size", r.URL.Query().Get("page_size"))
	if len(format) > 0 {
		h.writeError(w, r, format)
		return
	}
	res, err := h.queries.Purchases(r.Context(), userFrom(r.Context()), page, size)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := pageJSON{Items: make([]purchaseJSON, len(res.Items)), Page: res.Page, PageSize: res.PageSize, Total: res.Total}
	for i, p := range res.Items {
		out.Items[i] = toJSON(p)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func pageParam(errs *validation.Errors, field, raw string) *int {
	if raw == "" {
		return nil
	}
	n, err := catalog.ParseWholeNumber(raw)
	if err != nil {
		errs.Add(field, catalog.WholeNumberErrorCode(err), err.Error())
		return nil
	}
	value := int(n)
	return &value
}
