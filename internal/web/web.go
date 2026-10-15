package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
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
	Movements(ctx context.Context, productID string, p catalog.Paging) (catalog.MovementPage, error)
	PreviewImport(ctx context.Context, r io.Reader, opts catalog.ImportOptions) (string, error)
	ImportPreview(ctx context.Context, id string) (catalog.ImportPreview, error)
	ConfirmImport(ctx context.Context, id string, allowDuplicate bool) (catalog.ImportReport, error)
	CancelImport(ctx context.Context, id string) error
}

type Ordering interface {
	CartCount(ctx context.Context, userID string) (int, error)
	Cart(ctx context.Context, userID string) (ordering.Cart, error)
	AddToCart(ctx context.Context, userID, productID string, quantity int) error
	SetQuantity(ctx context.Context, userID, productID string, quantity int) error
	RemoveFromCart(ctx context.Context, userID, productID string) error
	AcceptCurrentPrice(ctx context.Context, userID, productID string) error
	CheckoutCart(ctx context.Context, userID, idempotencyKey, paymentToken string) (ordering.CheckoutResult, error)
	Purchase(ctx context.Context, userID, id string) (ordering.Purchase, error)
	Purchases(ctx context.Context, userID string, page, pageSize int) (ordering.PurchasePage, error)
}

type Deps struct {
	Catalog            Catalog
	Ordering           Ordering
	Users              []string
	Log                logger.Service
	MaxImportBodyBytes int64
}

type UI struct {
	catalog            Catalog
	orders             Ordering
	users              []string
	log                logger.Service
	maxImportBodyBytes int64
	views              *renderer
	static             http.Handler
}

func New(d Deps) (*UI, error) {
	if d.Catalog == nil {
		return nil, errors.New("web: catalog is required")
	}
	if d.Ordering == nil {
		return nil, errors.New("web: ordering is required")
	}
	if len(d.Users) == 0 {
		return nil, errors.New("web: demo users are required")
	}
	if d.Log == nil {
		return nil, errors.New("web: logger is required")
	}
	if d.MaxImportBodyBytes <= 0 {
		return nil, errors.New("web: import body limit is required")
	}
	views, err := newRenderer()
	if err != nil {
		return nil, err
	}
	static, err := staticFiles()
	if err != nil {
		return nil, err
	}
	return &UI{
		catalog: d.Catalog, orders: d.Ordering, users: append([]string(nil), d.Users...), log: d.Log,
		maxImportBodyBytes: d.MaxImportBodyBytes, views: views, static: static,
	}, nil
}

func (ui *UI) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(securityHeaders, ui.recoverHTML, ui.checkOrigin)
	r.NotFound(ui.notFound)
	r.MethodNotAllowed(ui.notFound)
	r.Group(func(r chi.Router) {
		r.Use(httpx.MaxBodyBytes(maxFormBytes))
		r.Handle("/static/*", ui.static)
		r.Get("/", ui.store)
		r.Get("/products/{id}", ui.product)
		r.Post("/demo-user", ui.selectUser)
		r.Get("/cart", ui.cart)
		r.Post("/cart/items", ui.addToCart)
		r.Post("/cart/items/{id}/quantity", ui.setQuantity)
		r.Post("/cart/items/{id}/remove", ui.removeFromCart)
		r.Post("/cart/items/{id}/accept-price", ui.acceptPrice)
		r.Get("/checkout", ui.checkoutForm)
		r.Post("/checkout", ui.checkout)
		r.Get("/purchases", ui.purchases)
		r.Get("/purchases/{id}", ui.purchase)
		r.Route(adminProducts, func(r chi.Router) {
			r.Get("/", ui.adminList)
			r.Post("/", ui.createProduct)
			r.Get("/new", ui.newProduct)
			r.Get("/{id}/edit", ui.editProduct)
			r.Post("/{id}", ui.updateProduct)
			r.Get("/{id}/movements", ui.movements)
			r.Get("/{id}/delete", ui.confirmDelete)
			r.Post("/{id}/delete", ui.deleteProduct)
		})
	})
	r.Group(func(r chi.Router) {
		r.Use(httpx.MaxBodyBytes(ui.maxImportBodyBytes))
		r.Get(adminImports, ui.importForm)
		r.Post(adminImports, ui.importFile)
		r.Get(adminImports+"/{id}", ui.importStatus)
		r.Post(adminImports+"/{id}/confirm", ui.confirmImport)
		r.Post(adminImports+"/{id}/cancel", ui.cancelImport)
	})
	return r
}

type errorPage struct {
	Title   string
	Status  int
	Message string
}

type header struct {
	Users     []string
	User      string
	Next      string
	CartCount int
	HasCount  bool
}

type view struct {
	Header header
	Data   any
}

func (ui *UI) notFound(w http.ResponseWriter, r *http.Request) {
	ui.errorPage(w, r, http.StatusNotFound, errorPage{
		Title: "Not found", Status: http.StatusNotFound,
		Message: "The page or product you are looking for does not exist.",
	})
}

func (ui *UI) serverError(w http.ResponseWriter, r *http.Request, err error) {
	ui.log.Error(r.Context(), err, map[string]any{"component": "web"})
	ui.errorPage(w, r, http.StatusInternalServerError, errorPage{
		Title: "Something went wrong", Status: http.StatusInternalServerError,
		Message: "An unexpected error occurred. Please try again.",
	})
}

func (ui *UI) page(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	ui.render(w, r, status, page, view{Header: ui.header(r, true), Data: data})
}

func (ui *UI) errorPage(w http.ResponseWriter, r *http.Request, status int, data errorPage) {
	ui.render(w, r, status, "error", view{Header: ui.header(r, false), Data: data})
}

func (ui *UI) render(w http.ResponseWriter, r *http.Request, status int, page string, v view) {
	if err := ui.views.render(w, status, page, "layout", v); err != nil {
		ui.log.Error(r.Context(), err, map[string]any{"component": "web"})
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

func (ui *UI) header(r *http.Request, withCount bool) header {
	h := header{Users: ui.users, User: ui.currentUser(r), Next: "/"}
	if r.Method == http.MethodGet {
		h.Next = localPath(r.URL.RequestURI())
	}
	if !withCount {
		return h
	}
	count, err := ui.orders.CartCount(r.Context(), h.User)
	if err != nil {
		ui.log.Error(r.Context(), fmt.Errorf("web: count the cart for the header: %w", err), map[string]any{"component": "web"})
		return h
	}
	h.CartCount, h.HasCount = count, true
	return h
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
			ui.errorPage(w, r, http.StatusInternalServerError, errorPage{
				Title: "Something went wrong", Status: http.StatusInternalServerError,
				Message: "An unexpected error occurred. Please try again.",
			})
		}()
		next.ServeHTTP(w, r)
	})
}
