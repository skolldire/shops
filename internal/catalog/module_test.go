package catalog

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
	"github.com/skolldire/shops/internal/platform/validation"
)

const productID = "2b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b"

type memoryRepo struct {
	products map[string]domain.Product
	search   domain.Search
}

func (m *memoryRepo) Create(_ context.Context, p domain.Product) (domain.Product, error) {
	for _, existing := range m.products {
		if existing.SKU == p.SKU {
			return domain.Product{}, app.ErrSKUTaken
		}
	}
	p.ID, p.Version = productID, 1
	m.products[p.ID] = p
	return p, nil
}

func (m *memoryRepo) Get(_ context.Context, id string) (domain.Product, error) {
	p, ok := m.products[id]
	if !ok {
		return domain.Product{}, app.ErrNotFound
	}
	return p, nil
}

func (m *memoryRepo) Update(_ context.Context, id string, version int, c domain.Changes) (domain.Product, error) {
	p, ok := m.products[id]
	switch {
	case !ok:
		return domain.Product{}, app.ErrNotFound
	case p.Version != version:
		return domain.Product{}, app.ErrVersionConflict
	}
	if c.Stock != nil {
		p.Stock = int(*c.Stock)
	}
	p.Version++
	m.products[id] = p
	return p, nil
}

func (m *memoryRepo) Delete(_ context.Context, id string, version int) error {
	p, ok := m.products[id]
	switch {
	case !ok:
		return app.ErrNotFound
	case p.Version != version:
		return app.ErrVersionConflict
	}
	delete(m.products, id)
	return nil
}

func (m *memoryRepo) Search(_ context.Context, s domain.Search) ([]domain.Product, int, error) {
	m.search = s
	var items []domain.Product
	for _, p := range m.products {
		items = append(items, p)
	}
	return items, len(items), nil
}

func (m *memoryRepo) Categories(context.Context) ([]string, error) {
	return []string{"Sports"}, nil
}

func testDeps(log logger.Service) Deps {
	return Deps{Log: log, Metrics: telemetry.Noop(), Tracer: telemetry.Noop(), Import: ImportLimits{MaxBytes: 1 << 20, MaxRows: 100}}
}

func (m *memoryRepo) Upsert(ctx context.Context, products []domain.Product) (map[string]app.UpsertOutcome, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]app.UpsertOutcome, len(products))
	for _, p := range products {
		out[p.SKU] = app.OutcomeCreated
		p.ID, p.Version = p.SKU, 1
		m.products[p.ID] = p
	}
	return out, nil
}

func (m *memoryRepo) HasProducts(context.Context) (bool, error) {
	return len(m.products) > 0, nil
}

func newTestModule(t *testing.T) (*Module, *memoryRepo) {
	t.Helper()
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	repo := &memoryRepo{products: map[string]domain.Product{}}
	m, err := newModule(repo, testDeps(log))
	require.NoError(t, err)
	return m, repo
}

func dec(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func i64(v int64) *int64 { return &v }

func validCreate() CreateInput {
	return CreateInput{SKU: "rs-001", Name: "Running Shoes", Category: "Sports", Price: dec("29.99"), Stock: i64(10), WeightKg: dec("0.850")}
}

func TestModuleLifecycle(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := context.Background()

	created, err := m.Create(ctx, validCreate())
	require.NoError(t, err)
	require.Equal(t, Product{
		ID: productID, SKU: "RS-001", Name: "Running Shoes", Category: "Sports",
		Price: *dec("29.99"), Stock: 10, WeightKg: *dec("0.850"), Version: 1,
	}, created)

	got, err := m.Get(ctx, productID)
	require.NoError(t, err)
	require.Equal(t, created, got)

	updated, err := m.Update(ctx, productID, UpdateInput{Version: 1, Stock: i64(3)})
	require.NoError(t, err)
	require.Equal(t, 3, updated.Stock)
	require.Equal(t, 2, updated.Version)

	require.NoError(t, m.Delete(ctx, productID, 2))
	_, err = m.Get(ctx, productID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestModuleTranslatesErrors(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := context.Background()
	_, err := m.Create(ctx, validCreate())
	require.NoError(t, err)

	_, err = m.Create(ctx, validCreate())
	require.ErrorIs(t, err, ErrSKUTaken)
	_, err = m.Update(ctx, productID, UpdateInput{Version: 9, Stock: i64(1)})
	require.ErrorIs(t, err, ErrVersionConflict)
	require.ErrorIs(t, m.Delete(ctx, "not-a-uuid", 1), ErrNotFound)

	_, err = m.Create(ctx, CreateInput{})
	var fieldErrs validation.Errors
	require.ErrorAs(t, err, &fieldErrs)
	require.Len(t, fieldErrs, 6)
}

func TestModuleSearch(t *testing.T) {
	m, repo := newTestModule(t)
	_, err := m.Create(context.Background(), validCreate())
	require.NoError(t, err)

	page, err := m.Search(context.Background(), SearchQuery{Q: " shoes ", Sort: "-price", Page: 2, PageSize: 5})

	require.NoError(t, err)
	require.Equal(t, "shoes", repo.search.Q)
	require.Equal(t, domain.SortPrice, repo.search.Sort)
	require.True(t, repo.search.Descending)
	require.Equal(t, 2, page.Page)
	require.Equal(t, 5, page.PageSize)
	require.Equal(t, 1, page.Total)
	require.Equal(t, "RS-001", page.Items[0].SKU)

	_, err = m.Search(context.Background(), SearchQuery{Sort: "stock"})
	var fieldErrs validation.Errors
	require.ErrorAs(t, err, &fieldErrs)
}

func TestParseSearchQuery(t *testing.T) {
	q, err := ParseSearchQuery(SearchParams{Q: "50%", Sort: "-created_at", InStock: "true", MinPrice: "10"})

	require.NoError(t, err)
	require.Equal(t, "50%", q.Q)
	require.Equal(t, "-created_at", q.Sort)
	require.True(t, q.InStock)
	require.Equal(t, "10", q.MinPrice.String())
	require.Equal(t, 1, q.Page)
	require.Equal(t, 20, q.PageSize)

	_, err = ParseSearchQuery(SearchParams{Page: "0"})
	var fieldErrs validation.Errors
	require.ErrorAs(t, err, &fieldErrs)
	require.Equal(t, "page", fieldErrs[0].Field)
}

func TestRegisterRoutes(t *testing.T) {
	m, _ := newTestModule(t)
	r := chi.NewRouter()
	r.Route("/api/v1", m.RegisterRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/categories", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `["Sports"]`, rec.Body.String())
}

func TestNewModuleRequiresDependencies(t *testing.T) {
	_, err := NewModule(Deps{})
	require.Error(t, err)

	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*Deps){
		"logger":  func(d *Deps) { d.Log = nil },
		"metrics": func(d *Deps) { d.Metrics = nil },
		"tracer":  func(d *Deps) { d.Tracer = nil },
		"limits":  func(d *Deps) { d.Import = ImportLimits{} },
	} {
		deps := testDeps(log)
		mutate(&deps)
		_, err = newModule(&memoryRepo{}, deps)
		require.Error(t, err, name)
	}
}
