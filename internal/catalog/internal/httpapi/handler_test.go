package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/catalog/internal/httpapi"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/validation"
)

const id = "2b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b"

var stamp = time.Date(2026, 10, 9, 17, 0, 0, 0, time.UTC)

func sample() core.Product {
	return core.Product{
		ID: id, SKU: "RS-001", Name: "Running Shoes", Description: "Light", Category: "Sports",
		Price: decimal.RequireFromString("29.9"), Stock: 10, WeightKg: decimal.RequireFromString("0.85"),
		Version: 3, CreatedAt: stamp, UpdatedAt: stamp.Add(5 * time.Minute),
	}
}

type fakeService struct {
	err     error
	created core.NewProductInput
	version int
	changes core.Changes
	search  core.Search
	cats    []string
}

func (f *fakeService) Create(_ context.Context, in core.NewProductInput) (core.Product, error) {
	f.created = in
	p := sample()
	p.Version = 1
	return p, f.err
}

func (f *fakeService) Get(context.Context, string) (core.Product, error) { return sample(), f.err }

func (f *fakeService) Update(_ context.Context, _ string, version int, c core.Changes) (core.Product, error) {
	f.version, f.changes = version, c
	p := sample()
	p.Version = version + 1
	return p, f.err
}

func (f *fakeService) Delete(_ context.Context, _ string, version int) error {
	f.version = version
	return f.err
}

func (f *fakeService) Search(_ context.Context, s core.Search) (core.Page, error) {
	f.search = s
	return core.Page{Items: []core.Product{sample()}, Page: s.Page, PageSize: s.PageSize, Total: 41}, f.err
}

func (f *fakeService) Categories(context.Context) ([]string, error) { return f.cats, f.err }

func newServer(t *testing.T, svc *fakeService) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	h, err := httpapi.New(svc, log)
	require.NoError(t, err)
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(httpx.MaxBodyBytes(1 << 20))
		h.Routes(r)
	})
	return r, &logs
}

func do(h http.Handler, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type problem struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Errors []struct {
		Field, Code, Message string
	} `json:"errors"`
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problem {
	t.Helper()
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"), rec.Body.String())
	var p problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	return p
}

func fieldCodes(p problem) map[string]string {
	out := map[string]string{}
	for _, fe := range p.Errors {
		out[fe.Field] = fe.Code
	}
	return out
}

const validBody = `{"sku":"rs-001","name":"Running Shoes","category":"Sports","price":"29.99","stock":10,"weight_kg":0.850}`

func TestCreate(t *testing.T) {
	svc := &fakeService{}
	h, _ := newServer(t, svc)

	rec := do(h, http.MethodPost, "/api/v1/products", validBody)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, "/api/v1/products/"+id, rec.Header().Get("Location"))
	require.Equal(t, `"1"`, rec.Header().Get("ETag"))
	require.JSONEq(t, `{"id":"`+id+`","sku":"RS-001","name":"Running Shoes","description":"Light","category":"Sports",
		"price":"29.90","stock":10,"weight_kg":"0.850","version":1,
		"created_at":"2026-10-09T17:00:00Z","updated_at":"2026-10-09T17:05:00Z"}`, rec.Body.String())
	require.Equal(t, "29.99", svc.created.Price.String())
	require.Equal(t, "0.85", svc.created.WeightKg.String())
	require.EqualValues(t, 10, *svc.created.Stock)
}

func TestCreateDecimalsNeverPassThroughFloat(t *testing.T) {
	svc := &fakeService{}
	h, _ := newServer(t, svc)
	body := `{"sku":"x","name":"n","category":"c","price":1234567890123.123456,"stock":1,"weight_kg":"123456.789"}`

	rec := do(h, http.MethodPost, "/api/v1/products", body)

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "1234567890123.123456", svc.created.Price.String())
	require.Equal(t, "123456.789", svc.created.WeightKg.String())
}

func TestGetReturnsETag(t *testing.T) {
	h, _ := newServer(t, &fakeService{})

	rec := do(h, http.MethodGet, "/api/v1/products/"+id, "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, `"3"`, rec.Header().Get("ETag"))
}

func TestUpdate(t *testing.T) {
	svc := &fakeService{}
	h, _ := newServer(t, svc)

	rec := do(h, http.MethodPatch, "/api/v1/products/"+id, `{"price":"12.50","stock":0}`, "If-Match", `"3"`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, `"4"`, rec.Header().Get("ETag"))
	require.Equal(t, 3, svc.version)
	require.Equal(t, "12.5", svc.changes.Price.String())
	require.EqualValues(t, 0, *svc.changes.Stock)
	require.Nil(t, svc.changes.Name)
}

func TestWeakIfMatchNeverMatches(t *testing.T) {
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			svc := &fakeService{}
			h, _ := newServer(t, svc)

			rec := do(h, method, "/api/v1/products/"+id, `{"stock":1}`, "If-Match", `W/"3"`)

			require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
			require.Equal(t, "precondition_failed", decodeProblem(t, rec).Code)
			require.Contains(t, rec.Body.String(), "weak entity tags are not accepted")
			require.Zero(t, svc.version, "a weak tag must not reach the catalog")
		})
	}
}

func TestDelete(t *testing.T) {
	svc := &fakeService{}
	h, _ := newServer(t, svc)

	rec := do(h, http.MethodDelete, "/api/v1/products/"+id, "", "If-Match", `"3"`)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Body.String())
	require.Equal(t, 3, svc.version)
}

func TestSearch(t *testing.T) {
	svc := &fakeService{}
	h, _ := newServer(t, svc)

	rec := do(h, http.MethodGet, "/api/v1/products?q=50%25&category=Audio&sort=-price&page=2&page_size=5", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "50%", svc.search.Q)
	require.Equal(t, core.SortPrice, svc.search.Sort)
	var page struct {
		Items    []map[string]any `json:"items"`
		Page     int              `json:"page"`
		PageSize int              `json:"page_size"`
		Total    int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, "29.90", page.Items[0]["price"])
	require.Equal(t, []int{2, 5, 41}, []int{page.Page, page.PageSize, page.Total})
}

func TestCategoriesNeverNull(t *testing.T) {
	h, _ := newServer(t, &fakeService{})
	rec := do(h, http.MethodGet, "/api/v1/categories", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `[]`, rec.Body.String())

	h, _ = newServer(t, &fakeService{cats: []string{"Audio", "Books"}})
	require.JSONEq(t, `["Audio","Books"]`, do(h, http.MethodGet, "/api/v1/categories", "").Body.String())
}

func TestErrorTable(t *testing.T) {
	big := `{"name":"` + strings.Repeat("x", 1<<20) + `"}`
	tests := map[string]struct {
		svcErr       error
		method, path string
		body         string
		headers      []string
		status       int
		code         string
	}{
		"malformed json":          {nil, http.MethodPost, "/api/v1/products", `{"sku":`, nil, 400, "invalid_request"},
		"unknown field":           {nil, http.MethodPost, "/api/v1/products", `{"sku":"a","colour":"red"}`, nil, 400, "invalid_request"},
		"trailing data":           {nil, http.MethodPost, "/api/v1/products", validBody + `{}`, nil, 400, "invalid_request"},
		"not an object":           {nil, http.MethodPost, "/api/v1/products", `[1,2]`, nil, 400, "invalid_request"},
		"body over 1 MB":          {nil, http.MethodPost, "/api/v1/products", big, nil, 413, "request_too_large"},
		"domain rules":            {fieldErr("name", "required"), http.MethodPost, "/api/v1/products", validBody, nil, 422, "validation_failed"},
		"sku in patch":            {fieldErr("sku", "immutable"), http.MethodPatch, "/api/v1/products/" + id, `{"sku":"X"}`, []string{"If-Match", `"3"`}, 422, "validation_failed"},
		"invalid search":          {nil, http.MethodGet, "/api/v1/products?page=0&sort=stock", "", nil, 422, "validation_failed"},
		"not found":               {core.ErrNotFound, http.MethodGet, "/api/v1/products/" + id, "", nil, 404, "not_found"},
		"sku taken":               {core.ErrSKUTaken, http.MethodPost, "/api/v1/products", validBody, nil, 409, "sku_taken"},
		"version conflict patch":  {core.ErrVersionConflict, http.MethodPatch, "/api/v1/products/" + id, `{"stock":1}`, []string{"If-Match", `"1"`}, 412, "version_conflict"},
		"version conflict delete": {core.ErrVersionConflict, http.MethodDelete, "/api/v1/products/" + id, "", []string{"If-Match", `"1"`}, 412, "version_conflict"},
		"patch without if-match":  {nil, http.MethodPatch, "/api/v1/products/" + id, `{"stock":1}`, nil, 428, "precondition_required"},
		"delete without if-match": {nil, http.MethodDelete, "/api/v1/products/" + id, "", nil, 428, "precondition_required"},
		"if-match wildcard":       {nil, http.MethodDelete, "/api/v1/products/" + id, "", []string{"If-Match", "*"}, 428, "precondition_required"},
		"if-match malformed":      {nil, http.MethodDelete, "/api/v1/products/" + id, "", []string{"If-Match", "3"}, 400, "invalid_request"},
		"if-match weak malformed": {nil, http.MethodDelete, "/api/v1/products/" + id, "", []string{"If-Match", "W/3"}, 400, "invalid_request"},
		"unexpected failure":      {errors.New("db exploded: host=10.0.0.5"), http.MethodGet, "/api/v1/products/" + id, "", nil, 500, "internal_error"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h, _ := newServer(t, &fakeService{err: tt.svcErr})

			rec := do(h, tt.method, tt.path, tt.body, tt.headers...)

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			p := decodeProblem(t, rec)
			require.Equal(t, tt.code, p.Code)
			require.NotContains(t, rec.Body.String(), "10.0.0.5")
		})
	}
}

func TestValidationReportsEveryFieldOnce(t *testing.T) {
	h, _ := newServer(t, &fakeService{})
	body := `{"sku":"bad sku","name":null,"category":7,"price":"cheap","stock":1.5,"weight_kg":"0.0001"}`

	rec := do(h, http.MethodPost, "/api/v1/products", body)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Equal(t, map[string]string{
		"sku":       "invalid_format",
		"name":      "invalid_format",
		"category":  "invalid_format",
		"price":     "invalid_format",
		"stock":     "invalid_format",
		"weight_kg": "too_many_decimals",
	}, fieldCodes(decodeProblem(t, rec)))
}

func TestStockOverflowIsOutOfRange(t *testing.T) {
	for _, stock := range []string{"2147483648", "-2147483649", "99999999999999999999"} {
		svc := &fakeService{}
		h, _ := newServer(t, svc)

		rec := do(h, http.MethodPost, "/api/v1/products", `{"sku":"a","name":"n","category":"c","price":1,"weight_kg":0,"stock":`+stock+`}`)

		require.Equal(t, map[string]string{"stock": "out_of_range"}, fieldCodes(decodeProblem(t, rec)), stock)
		require.Nil(t, svc.created.Stock, stock)
	}

	svc := &fakeService{}
	h, _ := newServer(t, svc)
	rec := do(h, http.MethodPost, "/api/v1/products", `{"sku":"a","name":"n","category":"c","price":1,"weight_kg":0,"stock":2147483647}`)
	require.Equal(t, http.StatusCreated, rec.Code)
	require.EqualValues(t, 2147483647, *svc.created.Stock)
}

func TestPatchPassesSKUToTheDomain(t *testing.T) {
	svc := &fakeService{err: fieldErr("sku", "immutable")}
	h, _ := newServer(t, svc)

	rec := do(h, http.MethodPatch, "/api/v1/products/"+id, `{"sku":"NEW"}`, "If-Match", `"3"`)

	require.NotNil(t, svc.changes.SKU)
	require.Equal(t, map[string]string{"sku": "immutable"}, fieldCodes(decodeProblem(t, rec)))
}

func TestPatchFormatAndDomainErrorsTogether(t *testing.T) {
	h, _ := newServer(t, &fakeService{})

	rec := do(h, http.MethodPatch, "/api/v1/products/"+id, `{"sku":"NEW","price":"0","stock":"ten"}`, "If-Match", `"3"`)

	require.Equal(t, map[string]string{"sku": "immutable", "price": "not_positive", "stock": "invalid_format"}, fieldCodes(decodeProblem(t, rec)))
}

func fieldErr(field, code string) error {
	var errs validation.Errors
	errs.Add(field, code, "invalid")
	return errs.Err()
}

func TestUnexpectedErrorsAreLogged(t *testing.T) {
	h, logs := newServer(t, &fakeService{err: errors.New("boom")})

	do(h, http.MethodGet, "/api/v1/categories", "")

	require.Contains(t, logs.String(), `"message":"boom"`)
}

func TestDecimalsMustBePlain(t *testing.T) {
	bodies := map[string]string{
		"string exponent":      `{"sku":"a","name":"n","category":"c","stock":1,"weight_kg":"1","price":"1e100000000"}`,
		"number exponent":      `{"sku":"a","name":"n","category":"c","stock":1,"weight_kg":"1","price":1e100000000}`,
		"small number exp":     `{"sku":"a","name":"n","category":"c","stock":1,"weight_kg":"1","price":2.999e1}`,
		"weight with exponent": `{"sku":"a","name":"n","category":"c","stock":1,"price":"1","weight_kg":"1E3"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			svc := &fakeService{}
			h, _ := newServer(t, svc)

			rec := within(t, func() *httptest.ResponseRecorder { return do(h, http.MethodPost, "/api/v1/products", body) })

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			decodeProblem(t, rec)
			require.Contains(t, rec.Body.String(), "must be a plain decimal number such as 29.99")
			require.Nil(t, svc.created.Price)
		})
	}

	h, _ := newServer(t, &fakeService{})
	rec := within(t, func() *httptest.ResponseRecorder {
		return do(h, http.MethodGet, "/api/v1/products?min_price=1e100000000", "")
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Equal(t, map[string]string{"min_price": "invalid_format"}, fieldCodes(decodeProblem(t, rec)))
}

func within(t *testing.T, fn func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- fn() }()
	select {
	case rec := <-done:
		return rec
	case <-time.After(100 * time.Millisecond):
		t.Fatal("request took longer than 100ms")
		return nil
	}
}
