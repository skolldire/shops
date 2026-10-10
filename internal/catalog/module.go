package catalog

import (
	"context"
	"errors"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/catalog/internal/httpapi"
	"github.com/skolldire/shops/internal/catalog/internal/postgres"
	"github.com/skolldire/shops/internal/platform/logger"
)

type Deps struct {
	Pool *pgxpool.Pool
	Log  logger.Service
}

type Module struct {
	svc  *core.Service
	http *httpapi.Handler
}

func NewModule(d Deps) (*Module, error) {
	repo, err := postgres.NewRepository(d.Pool)
	if err != nil {
		return nil, err
	}
	return newModule(repo, d.Log)
}

func newModule(repo core.Repository, log logger.Service) (*Module, error) {
	svc, err := core.NewService(repo)
	if err != nil {
		return nil, err
	}
	h, err := httpapi.New(svc, log)
	if err != nil {
		return nil, err
	}
	return &Module{svc: svc, http: h}, nil
}

func (m *Module) RegisterRoutes(r chi.Router) {
	m.http.Routes(r)
}

func ParseDecimal(raw string) (decimal.Decimal, error) {
	return core.ParseDecimal(raw)
}

func ParseSearchQuery(values url.Values) (SearchQuery, error) {
	s, err := core.ParseSearch(values)
	if err != nil {
		return SearchQuery{}, err
	}
	return fromSearch(s), nil
}

func (m *Module) Search(ctx context.Context, q SearchQuery) (Page, error) {
	s, err := core.NewSearch(core.SearchInput{
		Q: q.Q, Category: q.Category, MinPrice: q.MinPrice, MaxPrice: q.MaxPrice,
		InStock: q.InStock, Sort: q.Sort, Page: q.Page, PageSize: q.PageSize,
	})
	if err != nil {
		return Page{}, err
	}
	page, err := m.svc.Search(ctx, s)
	if err != nil {
		return Page{}, publicError(err)
	}
	out := Page{Items: make([]Product, len(page.Items)), Page: page.Page, PageSize: page.PageSize, Total: page.Total}
	for i, p := range page.Items {
		out.Items[i] = fromCore(p)
	}
	return out, nil
}

func (m *Module) Get(ctx context.Context, id string) (Product, error) {
	p, err := m.svc.Get(ctx, id)
	if err != nil {
		return Product{}, publicError(err)
	}
	return fromCore(p), nil
}

func (m *Module) Create(ctx context.Context, in CreateInput) (Product, error) {
	p, err := m.svc.Create(ctx, core.NewProductInput{
		SKU: in.SKU, Name: in.Name, Description: in.Description, Category: in.Category,
		Price: in.Price, Stock: in.Stock, WeightKg: in.WeightKg,
	})
	if err != nil {
		return Product{}, publicError(err)
	}
	return fromCore(p), nil
}

func (m *Module) Update(ctx context.Context, id string, in UpdateInput) (Product, error) {
	p, err := m.svc.Update(ctx, id, in.Version, core.Changes{
		SKU: in.SKU, Name: in.Name, Description: in.Description, Category: in.Category,
		Price: in.Price, Stock: in.Stock, WeightKg: in.WeightKg,
	})
	if err != nil {
		return Product{}, publicError(err)
	}
	return fromCore(p), nil
}

func (m *Module) Delete(ctx context.Context, id string, version int) error {
	return publicError(m.svc.Delete(ctx, id, version))
}

func (m *Module) Categories(ctx context.Context) ([]string, error) {
	categories, err := m.svc.Categories(ctx)
	if err != nil {
		return nil, publicError(err)
	}
	return categories, nil
}

func fromCore(p core.Product) Product {
	return Product{
		ID: p.ID, SKU: p.SKU, Name: p.Name, Description: p.Description, Category: p.Category,
		Price: p.Price, Stock: p.Stock, WeightKg: p.WeightKg, Version: p.Version,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func fromSearch(s core.Search) SearchQuery {
	sort := string(s.Sort)
	if s.Descending {
		sort = "-" + sort
	}
	return SearchQuery{
		Q: s.Q, Category: s.Category, MinPrice: s.MinPrice, MaxPrice: s.MaxPrice,
		InStock: s.InStock, Sort: sort, Page: s.Page, PageSize: s.PageSize,
	}
}

func publicError(err error) error {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, core.ErrSKUTaken):
		return ErrSKUTaken
	case errors.Is(err, core.ErrVersionConflict):
		return ErrVersionConflict
	}
	return err
}
