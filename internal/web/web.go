package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
)

const maxFormBytes = 1 << 20

var notices = map[string]string{
	"created": "Product created.",
	"updated": "Product updated.",
	"deleted": "Product deleted.",
}

type Catalog interface {
	Search(ctx context.Context, q catalog.SearchQuery) (catalog.Page, error)
	Get(ctx context.Context, id string) (catalog.Product, error)
	Create(ctx context.Context, in catalog.CreateInput) (catalog.Product, error)
	Update(ctx context.Context, id string, in catalog.UpdateInput) (catalog.Product, error)
	Delete(ctx context.Context, id string, version int) error
	Categories(ctx context.Context) ([]string, error)
}

type Deps struct {
	Catalog Catalog
	Log     logger.Service
}

type UI struct {
	catalog Catalog
	log     logger.Service
	views   *renderer
	static  http.Handler
}

func New(d Deps) (*UI, error) {
	if d.Catalog == nil {
		return nil, errors.New("web: catalog is required")
	}
	if d.Log == nil {
		return nil, errors.New("web: logger is required")
	}
	views, err := newRenderer()
	if err != nil {
		return nil, err
	}
	static, err := staticFiles()
	if err != nil {
		return nil, err
	}
	return &UI{catalog: d.Catalog, log: d.Log, views: views, static: static}, nil
}

func (ui *UI) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(securityHeaders, ui.recoverHTML, httpx.MaxBodyBytes(maxFormBytes))
	r.NotFound(ui.notFound)
	r.MethodNotAllowed(ui.notFound)
	r.Handle("/static/*", ui.static)
	r.Get("/", ui.store)
	r.Get("/products/{id}", ui.product)
	r.Route(adminProducts, func(r chi.Router) {
		r.Get("/", ui.adminList)
		r.Post("/", ui.createProduct)
		r.Get("/new", ui.newProduct)
		r.Get("/{id}/edit", ui.editProduct)
		r.Post("/{id}", ui.updateProduct)
		r.Get("/{id}/delete", ui.confirmDelete)
		r.Post("/{id}/delete", ui.deleteProduct)
	})
	return r
}

type errorPage struct {
	Title   string
	Status  int
	Message string
}

func (ui *UI) notFound(w http.ResponseWriter, r *http.Request) {
	ui.page(w, r, http.StatusNotFound, "error", errorPage{
		Title: "Not found", Status: http.StatusNotFound,
		Message: "The page or product you are looking for does not exist.",
	})
}

func (ui *UI) serverError(w http.ResponseWriter, r *http.Request, err error) {
	ui.log.Error(r.Context(), err, map[string]any{"component": "web"})
	ui.page(w, r, http.StatusInternalServerError, "error", errorPage{
		Title: "Something went wrong", Status: http.StatusInternalServerError,
		Message: "An unexpected error occurred. Please try again.",
	})
}

func (ui *UI) page(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	if err := ui.views.render(w, status, page, "layout", data); err != nil {
		ui.log.Error(r.Context(), err, map[string]any{"component": "web"})
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

func (ui *UI) recoverHTML(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			ui.log.Error(r.Context(), fmt.Errorf("panic recovered: %v", v), map[string]any{
				"component": "web",
				"stack":     strings.Split(strings.TrimSpace(string(debug.Stack())), "\n"),
			})
			ui.page(w, r, http.StatusInternalServerError, "error", errorPage{
				Title: "Something went wrong", Status: http.StatusInternalServerError,
				Message: "An unexpected error occurred. Please try again.",
			})
		}()
		next.ServeHTTP(w, r)
	})
}
