package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
)

type Service interface {
	Create(ctx context.Context, in core.NewProductInput) (core.Product, error)
	Get(ctx context.Context, id string) (core.Product, error)
	Update(ctx context.Context, id string, version int, c core.Changes) (core.Product, error)
	Delete(ctx context.Context, id string, version int) error
	Search(ctx context.Context, s core.Search) (core.Page, error)
	Categories(ctx context.Context) ([]string, error)
}

type Handler struct {
	svc Service
	log logger.Service
}

func New(svc Service, log logger.Service) (*Handler, error) {
	if svc == nil {
		return nil, errors.New("catalog: http handler requires a service")
	}
	if log == nil {
		return nil, errors.New("catalog: http handler requires a logger")
	}
	return &Handler{svc: svc, log: log}, nil
}

func (h *Handler) Routes(r chi.Router) {
	r.Get("/products", h.search)
	r.Post("/products", h.create)
	r.Get("/products/{id}", h.get)
	r.Patch("/products/{id}", h.update)
	r.Delete("/products/{id}", h.remove)
	r.Get("/categories", h.categories)
}

type productJSON struct {
	ID          string    `json:"id"`
	SKU         string    `json:"sku"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	Price       string    `json:"price"`
	Stock       int       `json:"stock"`
	WeightKg    string    `json:"weight_kg"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type pageJSON struct {
	Items    []productJSON `json:"items"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Total    int           `json:"total"`
}

func toJSON(p core.Product) productJSON {
	return productJSON{
		ID:          p.ID,
		SKU:         p.SKU,
		Name:        p.Name,
		Description: p.Description,
		Category:    p.Category,
		Price:       p.Price.StringFixed(2),
		Stock:       p.Stock,
		WeightKg:    p.WeightKg.StringFixed(3),
		Version:     p.Version,
		CreatedAt:   p.CreatedAt.UTC(),
		UpdatedAt:   p.UpdatedAt.UTC(),
	}
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	s, err := core.ParseSearch(r.URL.Query())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	page, err := h.svc.Search(r.Context(), s)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := pageJSON{Items: make([]productJSON, len(page.Items)), Page: page.Page, PageSize: page.PageSize, Total: page.Total}
	for i, p := range page.Items {
		out.Items[i] = toJSON(p)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	body, err := decodeBody(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	in, formatErrs := body.createInput()
	if len(formatErrs) > 0 {
		_, domainErr := core.NewProduct(in)
		h.writeError(w, r, merge(formatErrs, domainErr))
		return
	}
	p, err := h.svc.Create(r.Context(), in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", r.URL.Path+"/"+p.ID)
	h.writeProduct(w, http.StatusCreated, p)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeProduct(w, http.StatusOK, p)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	version, err := ifMatchVersion(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	changes, formatErrs := body.changes()
	if len(formatErrs) > 0 {
		_, domainErr := core.ValidateChanges(changes)
		h.writeError(w, r, merge(formatErrs, domainErr))
		return
	}
	p, err := h.svc.Update(r.Context(), chi.URLParam(r, "id"), version, changes)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeProduct(w, http.StatusOK, p)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	version, err := ifMatchVersion(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.svc.Delete(r.Context(), chi.URLParam(r, "id"), version); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) categories(w http.ResponseWriter, r *http.Request) {
	categories, err := h.svc.Categories(r.Context())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if categories == nil {
		categories = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, categories)
}

func (h *Handler) writeProduct(w http.ResponseWriter, status int, p core.Product) {
	w.Header().Set("ETag", etag(p.Version))
	httpx.WriteJSON(w, status, toJSON(p))
}
