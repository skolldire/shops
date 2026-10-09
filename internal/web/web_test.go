package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/logger"
)

type fakeCatalog struct {
	products   map[string]catalog.Product
	categories []string
	err        error
	writeErr   error
	total      int
	searched   bool
	query      catalog.SearchQuery
	created    catalog.CreateInput
	updated    catalog.UpdateInput
	deleted    int
}

func (f *fakeCatalog) Search(_ context.Context, q catalog.SearchQuery) (catalog.Page, error) {
	f.query, f.searched = q, true
	var items []catalog.Product
	for _, p := range f.products {
		items = append(items, p)
	}
	total := len(items)
	if f.total > 0 {
		total = f.total
	}
	return catalog.Page{Items: items, Page: q.Page, PageSize: q.PageSize, Total: total}, f.err
}

func (f *fakeCatalog) Get(_ context.Context, id string) (catalog.Product, error) {
	if f.err != nil {
		return catalog.Product{}, f.err
	}
	p, ok := f.products[id]
	if !ok {
		return catalog.Product{}, catalog.ErrNotFound
	}
	return p, nil
}

func (f *fakeCatalog) Create(_ context.Context, in catalog.CreateInput) (catalog.Product, error) {
	f.created = in
	return catalog.Product{ID: "new-id", SKU: in.SKU}, f.writeErr
}

func (f *fakeCatalog) Update(_ context.Context, id string, in catalog.UpdateInput) (catalog.Product, error) {
	f.updated = in
	return catalog.Product{ID: id}, f.writeErr
}

func (f *fakeCatalog) Delete(_ context.Context, _ string, version int) error {
	f.deleted = version
	return f.writeErr
}

func (f *fakeCatalog) Categories(context.Context) ([]string, error) {
	return f.categories, f.err
}

func newTestUI(t *testing.T, cat *fakeCatalog) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	if cat.products == nil {
		cat.products = map[string]catalog.Product{}
	}
	ui, err := New(Deps{Catalog: cat, Log: log})
	require.NoError(t, err)
	return ui.Routes(), &logs
}

func get(h http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var (
	inlineScript = regexp.MustCompile(`<script(?:\s[^>]*)?>`)
	inlineStyle  = regexp.MustCompile(`(?i)(<style|<[^>]*\s(style|on[a-z]+)=)`)
)

func requireSafeHTML(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, contentSecurityPolicy, rec.Header().Get("Content-Security-Policy"))
	body := rec.Body.String()
	for _, tag := range inlineScript.FindAllString(body, -1) {
		require.Contains(t, tag, `src="/static/`, "inline scripts are blocked by the CSP")
	}
	require.False(t, inlineStyle.MatchString(body), "inline styles and handlers are blocked by the CSP")
}

func TestNotFoundIsHTML(t *testing.T) {
	h, _ := newTestUI(t, &fakeCatalog{})

	for _, path := range []string{"/no-existe", "/static/missing.js"} {
		rec := get(h, path)

		require.Equal(t, http.StatusNotFound, rec.Code, path)
		if path == "/no-existe" {
			requireSafeHTML(t, rec)
			require.Contains(t, rec.Body.String(), "Not found")
		}
	}
}

func TestLayoutConfiguresHTMXForTheCSP(t *testing.T) {
	h, _ := newTestUI(t, &fakeCatalog{})

	body := get(h, "/no-existe").Body.String()

	require.Contains(t, body, `<meta name="htmx-config" content='{"includeIndicatorStyles":false,"allowEval":false,"historyCacheSize":0}'>`)
	require.Contains(t, body, `<script src="/static/htmx.min.js" defer></script>`)
	require.Contains(t, body, `<link rel="stylesheet" href="/static/app.css">`)
}

func TestStaticFiles(t *testing.T) {
	h, _ := newTestUI(t, &fakeCatalog{})

	js := get(h, "/static/htmx.min.js")
	require.Equal(t, http.StatusOK, js.Code)
	require.Contains(t, js.Header().Get("Content-Type"), "javascript")
	require.True(t, strings.HasPrefix(js.Body.String(), "var htmx="))

	css := get(h, "/static/app.css")
	require.Equal(t, http.StatusOK, css.Code)
	require.Contains(t, css.Header().Get("Content-Type"), "text/css")
	require.NotContains(t, css.Body.String(), "/*")
}

func TestPanicsRenderAnHTMLErrorPage(t *testing.T) {
	cat := &fakeCatalog{products: map[string]catalog.Product{}}
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	ui, err := New(Deps{Catalog: cat, Log: log})
	require.NoError(t, err)
	h := securityHeaders(ui.recoverHTML(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))

	rec := get(h, "/")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	requireSafeHTML(t, rec)
	require.Contains(t, rec.Body.String(), "Something went wrong")
	require.NotContains(t, rec.Body.String(), "boom")
	require.Contains(t, logs.String(), "panic recovered: boom")
}

func TestNewRequiresDependencies(t *testing.T) {
	_, err := New(Deps{})
	require.EqualError(t, err, "web: catalog is required")
	_, err = New(Deps{Catalog: &fakeCatalog{}})
	require.EqualError(t, err, "web: logger is required")
}
