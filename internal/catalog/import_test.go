package catalog

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/catalog/internal/app/apptest"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/logger"
)

const importHeader = "name,sku,description,category,price,stock,weight_kg\n"

func TestModuleImport(t *testing.T) {
	m, repo := newTestModule(t)
	body := importHeader + "Shoes,rs-1,Light,Sports,9.99,1,0.3\nMug,MG-1,,Home,free,0,0\n,,,,,,\n"

	report, err := m.Import(context.Background(), strings.NewReader(body), ImportOptions{FileName: "products.csv"})

	require.NoError(t, err)
	require.Equal(t, "products.csv", report.File.Name)
	require.EqualValues(t, len(body), report.File.Bytes)
	require.Len(t, report.File.SHA256, 64)
	require.Equal(t, []int{3, 1, 1, 0, 0, 1}, []int{report.Records, report.Skipped, report.Created, report.Updated, report.Unchanged, report.Rejected})
	require.Equal(t, []RowError{{Line: 3, SKU: "MG-1", Field: "price", Code: "invalid_format", Message: "must be a plain decimal number such as 29.99"}}, report.Errors)
	require.Contains(t, repo.products, "RS-1")
}

func TestModuleImportCleansTheFileName(t *testing.T) {
	m, _ := newTestModule(t)

	report, err := m.Import(context.Background(), strings.NewReader(importHeader), ImportOptions{FileName: "pro\tducts\x00.csv"})

	require.NoError(t, err)
	require.Equal(t, "products.csv", report.File.Name)
}

func TestModuleImportInterruptedReturnsTheReportWithTheError(t *testing.T) {
	m, repo := newTestModule(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := importHeader + "Shoes,RS-1,,Sports,9.99,1,0.3\nMug,MG-1,,Home,free,0,0\n"

	report, err := m.Import(ctx, strings.NewReader(body), ImportOptions{FileName: "products.csv"})

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, "products.csv", report.File.Name)
	require.Equal(t, []int{2, 0, 1}, []int{report.Records, report.Created, report.Rejected})
	require.Len(t, report.Errors, 1)
	require.Empty(t, repo.products)
}

func TestModuleImportErrorsArePublic(t *testing.T) {
	m, repo := newTestModule(t)
	tests := map[string]struct {
		body   string
		target error
	}{
		"header":    {"sku,name\n", ErrInvalidHeader},
		"csv":       {importHeader + "A,\"A-1,,A,5,1,1\n", ErrInvalidCSV},
		"rows":      {importHeader + strings.Repeat(",,,,,,\n", 101), ErrTooManyRows},
		"too large": {importHeader + strings.Repeat(" ", 1<<20), ErrFileTooLarge},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			report, err := m.Import(context.Background(), strings.NewReader(tt.body), ImportOptions{})

			require.ErrorIs(t, err, tt.target)
			require.Equal(t, ImportReport{}, report, "a file rejected as a whole has no report")
			require.Empty(t, repo.products)
		})
	}

	_, err := m.Import(context.Background(), strings.NewReader(importHeader+"A,A-1,,A,5,1,1\nB,\"B-1\n"), ImportOptions{})
	var invalid *InvalidCSVError
	require.ErrorAs(t, err, &invalid)
	require.Equal(t, 3, invalid.Line)
	require.Contains(t, invalid.Error(), "line 3")
}

func TestModuleSeed(t *testing.T) {
	const content = importHeader + "Shoes,RS-1,,Sports,9.99,1,0.3\nBad,BAD-1,,Sports,free,1,0.3\n"
	ctx := context.Background()

	t.Run("imports the file when there are no products", func(t *testing.T) {
		m, repo := newTestModule(t)

		report, seeded, err := m.Seed(ctx, strings.NewReader(content))

		require.NoError(t, err)
		require.True(t, seeded)
		require.Equal(t, "seed.csv", report.File.Name)
		require.Equal(t, 1, report.Created)
		require.Equal(t, 1, report.Rejected, "rejected rows are not a failure")
		require.Len(t, repo.products, 1)
	})

	t.Run("does nothing when there are products already", func(t *testing.T) {
		m, repo := newTestModule(t)
		repo.products["existing"] = domain.Product{ID: "existing", SKU: "OLD-1"}
		file := strings.NewReader(content)

		report, seeded, err := m.Seed(ctx, file)

		require.NoError(t, err)
		require.False(t, seeded)
		require.Equal(t, ImportReport{}, report)
		require.Len(t, repo.products, 1)
		require.Equal(t, len(content), file.Len(), "the file is not read")
	})

	t.Run("returns what was applied when the import is interrupted", func(t *testing.T) {
		m, repo := newTestModule(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()

		report, seeded, err := m.Seed(cancelled, strings.NewReader(content))

		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, "catalog: import seed file: ")
		require.False(t, seeded)
		require.Equal(t, []int{2, 0, 1}, []int{report.Records, report.Created, report.Rejected})
		require.Equal(t, "seed.csv", report.File.Name)
		require.Empty(t, repo.products)
	})

	t.Run("fails when the file is not a valid import", func(t *testing.T) {
		m, _ := newTestModule(t)

		report, seeded, err := m.Seed(ctx, strings.NewReader("sku,name\n"))

		require.ErrorIs(t, err, ErrInvalidHeader)
		require.Equal(t, ImportReport{}, report, "a file rejected as a whole has no report")
		require.False(t, seeded)
	})
}

func TestModuleImportRouteTranslatesFileRejections(t *testing.T) {
	post := func(content string) *httptest.ResponseRecorder {
		m, _ := newTestModule(t)
		r := chi.NewRouter()
		r.Route("/api/v1", m.RegisterImportRoutes)
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("file", "products.csv")
		require.NoError(t, err)
		_, err = part.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, form.Close())
		req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	tests := map[string]struct {
		content string
		status  int
		code    string
	}{
		"another header": {"sku,name\n", 400, "invalid_header"},
		"malformed csv":  {importHeader + "A,\"A-1,,A,5,1,1\n", 400, "invalid_csv"},
		"too many rows":  {importHeader + strings.Repeat(",,,,,,\n", 101), 400, "too_many_rows"},
		"too large":      {importHeader + strings.Repeat(" ", 1<<20), 413, "request_too_large"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rec := post(tt.content)

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), `"code":"`+tt.code+`"`)
		})
	}

	ok := post(importHeader + "Shoes,RS-1,,Sports,9.99,1,0.3\n")
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	require.Contains(t, ok.Body.String(), `"created":1`)
}

func TestModuleRegistersImportRoutes(t *testing.T) {
	m, _ := newTestModule(t)
	r := chi.NewRouter()
	r.Route("/api/v1", m.RegisterImportRoutes)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/imports", strings.NewReader("x")))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid_request")
}

type spanRecorder struct {
	names []string
	open  []string
	paths []string
}

func (r *spanRecorder) Span(ctx context.Context, name string, fn func(ctx context.Context) error, _ ...attribute.KeyValue) error {
	r.names = append(r.names, name)
	r.open = append(r.open, name)
	r.paths = append(r.paths, strings.Join(r.open, " > "))
	err := fn(ctx)
	r.open = r.open[:len(r.open)-1]
	return err
}

func TestModuleImportIsTracedAsOneSpan(t *testing.T) {
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	spans := &spanRecorder{}
	deps := testDeps(log)
	deps.Tracer = spans
	m, err := newModule(&memoryRepo{products: map[string]domain.Product{}}, apptest.NewLog(), apptest.Tx{}, deps)
	require.NoError(t, err)

	_, err = m.Import(context.Background(), strings.NewReader(importHeader+"Shoes,RS-1,,Sports,9.99,1,0.3\n"), ImportOptions{FileName: "products.csv"})
	require.NoError(t, err)
	require.Equal(t, []string{"catalog.import"}, spans.paths, "one span for the whole import")

	spans.paths = nil
	_, err = m.Import(context.Background(), strings.NewReader("sku,name\n"), ImportOptions{})
	require.ErrorIs(t, err, ErrInvalidHeader)
	require.Equal(t, []string{"catalog.import"}, spans.paths, "a rejected file is traced the same way")
}

type meter struct {
	histograms map[string]int
	counters   map[string]int64
}

func (m *meter) Counter(_ context.Context, name string, value int64, attrs ...attribute.KeyValue) {
	for _, attr := range attrs {
		name += "{" + string(attr.Key) + "=" + attr.Value.AsString() + "}"
	}
	m.counters[name] += value
}

func (m *meter) Gauge(context.Context, string, float64, ...attribute.KeyValue) {}

func (m *meter) Histogram(_ context.Context, name string, _ float64, _ ...attribute.KeyValue) {
	m.histograms[name]++
}

func TestModuleImportRecordsItsDurationOnEveryAttempt(t *testing.T) {
	valid := importHeader + "Shoes,RS-1,,Sports,9.99,1,0.3\nBad,BAD-1,,Sports,free,1,0.3\n"
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := map[string]struct {
		ctx     context.Context
		body    string
		target  error
		created int64
	}{
		"success":                {context.Background(), valid, nil, 1},
		"invalid header":         {context.Background(), "sku,name\n", ErrInvalidHeader, 0},
		"cancelled before apply": {cancelled, valid, context.Canceled, 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
			require.NoError(t, err)
			metrics := &meter{histograms: map[string]int{}, counters: map[string]int64{}}
			deps := testDeps(log)
			deps.Metrics = metrics
			m, err := newModule(&memoryRepo{products: map[string]domain.Product{}}, apptest.NewLog(), apptest.Tx{}, deps)
			require.NoError(t, err)

			report, err := m.Import(tt.ctx, strings.NewReader(tt.body), ImportOptions{FileName: "products.csv"})

			if tt.target == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.target)
			}
			require.Equal(t, map[string]int{"import_duration_seconds": 1}, metrics.histograms, "one duration per attempt, whatever its outcome")
			require.Equal(t, tt.created, metrics.counters["import_rows_total{outcome=created}"])
			if tt.target == context.Canceled {
				require.Equal(t, 2, report.Records, "the result reflects what was read before the interruption")
				require.Zero(t, report.Created)
				require.EqualValues(t, 1, metrics.counters["import_rows_total{outcome=rejected}"], "only confirmed outcomes are counted")
			}
		})
	}
}
