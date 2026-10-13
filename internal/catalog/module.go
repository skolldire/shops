package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/csvimport"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/catalog/internal/httpapi"
	"github.com/skolldire/shops/internal/catalog/internal/httpquery"
	"github.com/skolldire/shops/internal/catalog/internal/observe"
	"github.com/skolldire/shops/internal/catalog/internal/postgres"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

type Deps struct {
	Pool    *pgxpool.Pool
	Log     logger.Service
	Metrics telemetry.Metrics
	Tracer  telemetry.Tracer
	Import  ImportLimits
}

const seedFileName = "seed.csv"

type seedGuard interface {
	HasProducts(ctx context.Context) (bool, error)
}

type store interface {
	app.Repository
	app.ProductUpserter
	seedGuard
}

type Module struct {
	svc      *app.Service
	importer *observe.InstrumentedImporter
	seed     seedGuard
	http     *httpapi.Handler
	imports  *httpapi.ImportHandler
}

func NewModule(d Deps) (*Module, error) {
	repo, err := postgres.NewRepository(d.Pool)
	if err != nil {
		return nil, err
	}
	return newModule(repo, d)
}

func newModule(s store, d Deps) (*Module, error) {
	svc, err := app.NewService(s)
	if err != nil {
		return nil, err
	}
	h, err := httpapi.New(svc, d.Log)
	if err != nil {
		return nil, err
	}
	recorder, err := observe.NewImportRecorder(d.Metrics)
	if err != nil {
		return nil, err
	}
	failures, err := observe.NewFailureReporter(d.Log)
	if err != nil {
		return nil, err
	}
	rows, err := app.NewImporter(s, recorder, failures)
	if err != nil {
		return nil, err
	}
	files, err := csvimport.NewImporter(rows, csvimport.Limits{MaxBytes: d.Import.MaxBytes, MaxRows: d.Import.MaxRows})
	if err != nil {
		return nil, err
	}
	importer, err := observe.NewInstrumentedImporter(files, d.Tracer, d.Metrics, csvimport.IsRejection)
	if err != nil {
		return nil, err
	}
	imports, err := httpapi.NewImports(importer, d.Log)
	if err != nil {
		return nil, err
	}
	return &Module{svc: svc, importer: importer, seed: s, http: h, imports: imports}, nil
}

func (m *Module) RegisterRoutes(r chi.Router) {
	m.http.Routes(r)
}

func (m *Module) RegisterImportRoutes(r chi.Router) {
	m.imports.Routes(r)
}

func (m *Module) Import(ctx context.Context, r io.Reader, opts ImportOptions) (ImportReport, error) {
	report, err := m.importer.Import(ctx, r, opts.FileName)
	switch {
	case err == nil:
		return fromReport(report), nil
	case report.Errors == nil:
		return ImportReport{}, publicImportError(err)
	}
	return fromReport(report), err
}

func (m *Module) Seed(ctx context.Context, file io.Reader) (ImportReport, bool, error) {
	has, err := m.seed.HasProducts(ctx)
	if err != nil {
		return ImportReport{}, false, err
	}
	if has {
		return ImportReport{}, false, nil
	}
	report, err := m.Import(ctx, file, ImportOptions{FileName: seedFileName})
	if err != nil {
		return report, false, fmt.Errorf("catalog: import seed file: %w", err)
	}
	return report, true, nil
}

func ParseDecimal(raw string) (decimal.Decimal, error) {
	return domain.ParseDecimal(raw)
}

func ParseWholeNumber(raw string) (int64, error) {
	return domain.ParseWholeNumber(raw)
}

func ParseSearchQuery(p SearchParams) (SearchQuery, error) {
	s, err := httpquery.ParseParams(httpquery.Params(p))
	if err != nil {
		return SearchQuery{}, err
	}
	return fromSearch(s), nil
}

func (m *Module) Search(ctx context.Context, q SearchQuery) (Page, error) {
	s, err := domain.NewSearch(domain.SearchInput{
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
	p, err := m.svc.Create(ctx, domain.NewProductInput{
		SKU: in.SKU, Name: in.Name, Description: in.Description, Category: in.Category,
		Price: in.Price, Stock: in.Stock, WeightKg: in.WeightKg,
	})
	if err != nil {
		return Product{}, publicError(err)
	}
	return fromCore(p), nil
}

func (m *Module) Update(ctx context.Context, id string, in UpdateInput) (Product, error) {
	p, err := m.svc.Update(ctx, id, in.Version, domain.Changes{
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

func fromCore(p domain.Product) Product {
	return Product{
		ID: p.ID, SKU: p.SKU, Name: p.Name, Description: p.Description, Category: p.Category,
		Price: p.Price, Stock: p.Stock, WeightKg: p.WeightKg, Version: p.Version,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func fromSearch(s domain.Search) SearchQuery {
	sort := string(s.Sort)
	if s.Descending {
		sort = "-" + sort
	}
	return SearchQuery{
		Q: s.Q, Category: s.Category, MinPrice: s.MinPrice, MaxPrice: s.MaxPrice,
		InStock: s.InStock, Sort: sort, Page: s.Page, PageSize: s.PageSize,
	}
}

func fromReport(r app.ImportReport) ImportReport {
	out := ImportReport{
		File:    ImportFile{Name: r.File.Name, Bytes: r.File.Bytes, SHA256: r.File.SHA256},
		Records: r.Records, Skipped: r.Skipped, Created: r.Created, Updated: r.Updated,
		Unchanged: r.Unchanged, Rejected: r.Rejected,
		Errors: make([]RowError, len(r.Errors)),
	}
	for i, e := range r.Errors {
		out.Errors[i] = RowError{Line: e.Line, SKU: e.SKU, Field: e.Field, Code: e.Code, Message: e.Message}
	}
	return out
}

func publicImportError(err error) error {
	var format *csvimport.FormatError
	switch {
	case errors.Is(err, csvimport.ErrFileTooLarge):
		return ErrFileTooLarge
	case errors.Is(err, csvimport.ErrInvalidHeader):
		return ErrInvalidHeader
	case errors.Is(err, csvimport.ErrTooManyRows):
		return ErrTooManyRows
	case errors.As(err, &format):
		return &InvalidCSVError{Line: format.Line, Reason: format.Reason}
	}
	return err
}

func publicError(err error) error {
	switch {
	case errors.Is(err, app.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, app.ErrSKUTaken):
		return ErrSKUTaken
	case errors.Is(err, app.ErrVersionConflict):
		return ErrVersionConflict
	}
	return err
}
