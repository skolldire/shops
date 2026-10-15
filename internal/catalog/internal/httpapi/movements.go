package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/catalog/internal/httpquery"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
)

type Inventory interface {
	Movements(ctx context.Context, productID string, p domain.Paging) (app.MovementPage, error)
}

type MovementsHandler struct {
	errors    *Handler
	inventory Inventory
}

func NewMovements(inventory Inventory, log logger.Service) (*MovementsHandler, error) {
	if inventory == nil {
		return nil, errors.New("catalog: movements handler requires an inventory")
	}
	if log == nil {
		return nil, errors.New("catalog: movements handler requires a logger")
	}
	return &MovementsHandler{errors: &Handler{log: log}, inventory: inventory}, nil
}

func (h *MovementsHandler) Routes(r chi.Router) {
	r.Get("/products/{id}/movements", h.list)
}

type movementJSON struct {
	ID         int64     `json:"id"`
	ProductID  string    `json:"product_id"`
	Delta      int       `json:"delta"`
	StockAfter int       `json:"stock_after"`
	Reason     string    `json:"reason"`
	Reference  *string   `json:"reference"`
	DBUser     string    `json:"db_user"`
	TxID       string    `json:"tx_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type movementPageJSON struct {
	Items    []movementJSON `json:"items"`
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
	Total    int            `json:"total"`
}

func (h *MovementsHandler) list(w http.ResponseWriter, r *http.Request) {
	paging, err := httpquery.ParsePaging(r.URL.Query())
	if err != nil {
		h.errors.writeError(w, r, err)
		return
	}
	page, err := h.inventory.Movements(r.Context(), chi.URLParam(r, "id"), paging)
	if err != nil {
		h.errors.writeError(w, r, err)
		return
	}
	out := movementPageJSON{Items: make([]movementJSON, len(page.Items)), Page: page.Page, PageSize: page.PageSize, Total: page.Total}
	for i, m := range page.Items {
		out.Items[i] = movementJSON{
			ID: m.ID, ProductID: m.ProductID, Delta: m.Delta, StockAfter: m.StockAfter, Reason: m.Reason,
			DBUser: m.DBUser, TxID: m.TxID, CreatedAt: m.CreatedAt.UTC(),
		}
		if m.Reference != "" {
			out.Items[i].Reference = &m.Reference
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
