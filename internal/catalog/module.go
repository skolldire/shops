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
	"github.com/skolldire/shops/internal/platform/database"
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
	LockSeed(ctx context.Context) (context.Context, func(), error)
	HasProducts(ctx context.Context) (bool, error)
}

type store interface {
	app.Repository
	app.ProductUpserter
	app.StockStore
	seedGuard
}

type Module struct {
	svc       *app.Service
	inventory *app.Inventory
	importer  *observe.InstrumentedImporter
	seed      seedGuard
	http      *httpapi.Handler
	movements *httpapi.MovementsHandler
	imports   *httpapi.ImportHandler
}

func NewModule(d Deps) (*Module, error) {
	repo, err := postgres.NewRepository(d.Pool)
	if err != nil {
		return nil, err
	}
	log, err := postgres.NewImportLog(d.Pool)
	if err != nil {
		return nil, err
	}
	tx, err := database.NewTxManager(d.Pool)
	if err != nil {
		return nil, err
	}
	return newModule(repo, log, tx, d)
}

func newModule(s store, log app.ImportLog, tx app.Tx, d Deps) (*Module, error) {
	svc, err := app.NewService(s)
	if err != nil {
		return nil, err
	}
	inventory, err := app.NewInventory(s)
	if err != nil {
		return nil, err
	}
	h, err := httpapi.New(svc, d.Log)
	if err != nil {
		return nil, err
	}
	movements, err := httpapi.NewMovements(inventory, d.Log)
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
	rows, err := app.NewImporter(s, log, tx, recorder, failures)
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
	return &Module{
		svc: svc, inventory: inventory, importer: importer,
		seed: s, http: h, movements: movements, imports: imports,
	}, nil
}

func (m *Module) RegisterRoutes(r chi.Router) {
	m.http.Routes(r)
	m.movements.Routes(r)
}

func (m *Module) RegisterImportRoutes(r chi.Router) {
	m.imports.Routes(r)
}

func (m *Module) Import(ctx context.Context, r io.Reader, opts ImportOptions) (ImportReport, error) {
	mode, err := domain.ParseImportMode(string(opts.Mode))
	if err != nil {
		return ImportReport{}, ErrInvalidImportMode
	}
	report, err := m.importer.Import(ctx, r, app.ImportOptions{
		FileName: opts.FileName, Mode: mode, Key: opts.IdempotencyKey, AllowDuplicate: opts.AllowDuplicate,
	})
	switch {
	case err == nil:
		return fromReport(report), nil
	case report.Errors == nil:
		return ImportReport{}, publicImportError(err)
	}
	return fromReport(report), err
}

func (m *Module) Seed(ctx context.Context, file io.Reader) (ImportReport, bool, error) {
	ctx, unlock, err := m.seed.LockSeed(ctx)
	if err != nil {
		return ImportReport{}, false, err
	}
	defer unlock()
	has, err := m.seed.HasProducts(ctx)
	if err != nil {
		return ImportReport{}, false, err
	}
	if has {
		return ImportReport{}, false, nil
	}
	report, err := m.Import(ctx, file, ImportOptions{FileName: seedFileName, Mode: ImportReplace})
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

const ImportRetryAfterSeconds = app.ImportRetryAfterSeconds

func WholeNumberErrorCode(err error) string {
	return domain.WholeNumberErrorCode(err)
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

func (m *Module) ReserveStock(ctx context.Context, items []StockRequest, reference string) ([]StockLine, error) {
	lines, err := m.inventory.ReserveStock(ctx, toStockRequests(items), reference)
	if err != nil {
		return nil, publicError(err)
	}
	out := make([]StockLine, len(lines))
	for i, l := range lines {
		out[i] = StockLine{ProductID: l.ProductID, SKU: l.SKU, Name: l.Name, Price: l.Price, Quantity: l.Quantity}
	}
	return out, nil
}

func (m *Module) ReleaseStock(ctx context.Context, items []StockRequest, reference string) error {
	return publicError(m.inventory.ReleaseStock(ctx, toStockRequests(items), reference))
}

func (m *Module) CommitStock(ctx context.Context, items []StockRequest, reference string) error {
	return publicError(m.inventory.CommitStock(ctx, toStockRequests(items), reference))
}

func ParsePaging(page, pageSize string) (Paging, error) {
	p, err := httpquery.ParsePagingParams(page, pageSize)
	if err != nil {
		return Paging{}, err
	}
	return Paging{Page: p.Page, PageSize: p.PageSize}, nil
}

func (m *Module) Movements(ctx context.Context, productID string, p Paging) (MovementPage, error) {
	paging, err := domain.NewPaging(p.Page, p.PageSize)
	if err != nil {
		return MovementPage{}, err
	}
	page, err := m.inventory.Movements(ctx, productID, paging)
	if err != nil {
		return MovementPage{}, publicError(err)
	}
	out := MovementPage{Items: make([]Movement, len(page.Items)), Page: page.Page, PageSize: page.PageSize, Total: page.Total}
	for i, mv := range page.Items {
		out.Items[i] = Movement{
			ID: mv.ID, ProductID: mv.ProductID, Delta: mv.Delta, StockAfter: mv.StockAfter,
			ReservedDelta: mv.ReservedDelta, ReservedAfter: mv.ReservedAfter, Reason: mv.Reason,
			Reference: mv.Reference, DBUser: mv.DBUser, TxID: mv.TxID, CreatedAt: mv.CreatedAt,
		}
	}
	return out, nil
}

func toStockRequests(items []StockRequest) []domain.StockRequest {
	out := make([]domain.StockRequest, len(items))
	for i, it := range items {
		out[i] = domain.StockRequest{ProductID: it.ProductID, Quantity: it.Quantity}
	}
	return out
}

func fromCore(p domain.Product) Product {
	return Product{
		ID: p.ID, SKU: p.SKU, Name: p.Name, Description: p.Description, Category: p.Category,
		Price: p.Price, Stock: p.Stock, Reserved: p.Reserved, Available: p.Available(), WeightKg: p.WeightKg, Version: p.Version,
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
		ID: r.ID, Mode: ImportMode(r.Mode), Changes: make([]ImportChange, len(r.Changes)),
		File:    ImportFile{Name: r.File.Name, Bytes: r.File.Bytes, SHA256: r.File.SHA256},
		Records: r.Records, Skipped: r.Skipped, Created: r.Created, Updated: r.Updated,
		Unchanged: r.Unchanged, Rejected: r.Rejected,
		Errors: make([]RowError, len(r.Errors)),
	}
	for i, e := range r.Errors {
		out.Errors[i] = RowError{Line: e.Line, SKU: e.SKU, Field: e.Field, Code: e.Code, Message: e.Message}
	}
	for i, c := range r.Changes {
		out.Changes[i] = ImportChange{SKU: c.SKU, StockBefore: c.StockBefore, StockAfter: c.StockAfter, PriceBefore: c.PriceBefore, PriceAfter: c.PriceAfter}
	}
	return out
}

func publicImportError(err error) error {
	var (
		format    *csvimport.FormatError
		duplicate *app.DuplicateFileError
	)
	switch {
	case errors.As(err, &duplicate):
		return &DuplicateFileError{AppliedAt: duplicate.Previous.AppliedAt}
	case errors.Is(err, csvimport.ErrFileTooLarge):
		return ErrFileTooLarge
	case errors.Is(err, csvimport.ErrInvalidHeader):
		return ErrInvalidHeader
	case errors.Is(err, csvimport.ErrTooManyRows):
		return ErrTooManyRows
	case errors.As(err, &format):
		return &InvalidCSVError{Line: format.Line, Reason: format.Reason}
	case errors.Is(err, app.ErrInvalidIdempotencyKey):
		return ErrInvalidIdempotencyKey
	case errors.Is(err, app.ErrIdempotencyKeyRequired):
		return ErrIdempotencyKeyRequired
	case errors.Is(err, app.ErrIdempotencyConflict):
		return ErrIdempotencyConflict
	case errors.Is(err, app.ErrImportInProgress):
		return ErrImportInProgress
	}
	return err
}

func publicError(err error) error {
	var (
		short       *app.InsufficientStockError
		unavailable *app.ProductUnavailableError
		invariant   *app.StockInvariantError
		below       *app.BelowReservedError
	)
	switch {
	case errors.As(err, &short):
		return &InsufficientStockError{ProductID: short.ProductID, Available: short.Available}
	case errors.As(err, &unavailable):
		return &ProductUnavailableError{ProductID: unavailable.ProductID}
	case errors.Is(err, app.ErrInvalidStockRequest):
		return ErrInvalidStockRequest
	case errors.As(err, &below):
		return &BelowReservedError{Reserved: below.Reserved, Message: below.Message()}
	case errors.As(err, &invariant):
		return fmt.Errorf("%w: product %s has fewer than %d reserved units to settle", ErrStockInvariant, invariant.ProductID, invariant.Quantity)
	case errors.Is(err, app.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, app.ErrSKUTaken):
		return ErrSKUTaken
	case errors.Is(err, app.ErrImportInProgress):
		return ErrImportInProgress
	case errors.Is(err, app.ErrVersionConflict):
		return ErrVersionConflict
	}
	return err
}
