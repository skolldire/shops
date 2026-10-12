package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/httpapi"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
)

type fakeImporter struct {
	report app.ImportReport
	err    error
	calls  int
	name   string
	body   string
}

func (f *fakeImporter) Import(_ context.Context, r io.Reader, fileName string) (app.ImportReport, error) {
	f.calls++
	f.name = fileName
	body, err := io.ReadAll(r)
	f.body = string(body)
	if err != nil {
		return app.ImportReport{}, fmt.Errorf("catalog: read import file: %w", err)
	}
	return f.report, f.err
}

func newImportServer(t *testing.T, importer *fakeImporter, maxBody int64) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	h, err := httpapi.NewImports(importer, log)
	require.NoError(t, err)
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(httpx.MaxBodyBytes(maxBody))
		h.Routes(r)
	})
	return r, &logs
}

func upload(t *testing.T, h http.Handler, field, name, content string, extra ...string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for i := 0; i+1 < len(extra); i += 2 {
		require.NoError(t, form.WriteField(extra[i], extra[i+1]))
	}
	part, err := form.CreateFormFile(field, name)
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, form.Close())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestImportReturnsTheReportEvenWithRejectedRows(t *testing.T) {
	importer := &fakeImporter{report: app.ImportReport{
		File:    app.ImportFile{Name: "products.csv", Bytes: 10240, SHA256: "6fc46b84"},
		Records: 97, Skipped: 2, Created: 80, Updated: 1, Unchanged: 1, Rejected: 13,
		Errors: []app.RowError{
			{Line: 12, SKU: "BS-021", Field: "sku", Code: "duplicate_in_file", Message: "also used on lines 57 and 90"},
			{Line: 15, SKU: "XY-003", Field: "price", Code: "invalid_format", Message: "must be a plain decimal number such as 29.99"},
		},
	}}
	h, _ := newImportServer(t, importer, 1<<20)

	rec := upload(t, h, "file", "products.csv", "name,sku\n", "note", "ignored")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{
		"file": {"name": "products.csv", "bytes": 10240, "sha256": "6fc46b84"},
		"records": 97, "skipped": 2, "created": 80, "updated": 1, "unchanged": 1, "rejected": 13,
		"errors": [
			{"line": 12, "sku": "BS-021", "field": "sku", "code": "duplicate_in_file", "message": "also used on lines 57 and 90"},
			{"line": 15, "sku": "XY-003", "field": "price", "code": "invalid_format", "message": "must be a plain decimal number such as 29.99"}
		]
	}`, rec.Body.String())
	require.Equal(t, "products.csv", importer.name)
	require.Equal(t, "name,sku\n", importer.body)
}

func TestImportWithoutErrorsReturnsAnEmptyList(t *testing.T) {
	h, _ := newImportServer(t, &fakeImporter{report: app.ImportReport{Errors: []app.RowError{}}}, 1<<20)

	rec := upload(t, h, "file", "products.csv", "name\n")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"errors":[]`)
}

func TestImportErrorTable(t *testing.T) {
	tests := map[string]struct {
		err    error
		status int
		code   string
		detail string
	}{
		"another header":     {app.ErrInvalidHeader, 400, "invalid_header", "name,sku,description,category,price,stock,weight_kg"},
		"malformed csv":      {&app.InvalidCSVError{Line: 7, Reason: "bare quote"}, 400, "invalid_csv", "Line 7: bare quote."},
		"too many rows":      {app.ErrTooManyRows, 400, "too_many_rows", "more rows"},
		"file too large":     {app.ErrFileTooLarge, 413, "request_too_large", "larger than the import limit"},
		"unexpected failure": {errors.New("db exploded: host=10.0.0.5"), 500, "internal_error", "unexpected"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h, logs := newImportServer(t, &fakeImporter{err: tt.err}, 1<<20)

			rec := upload(t, h, "file", "products.csv", "name\n")

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.Equal(t, tt.code, decodeProblem(t, rec).Code)
			require.Contains(t, rec.Body.String(), tt.detail)
			require.NotContains(t, rec.Body.String(), "10.0.0.5")
			if tt.status == 500 {
				require.Contains(t, logs.String(), "db exploded")
			}
		})
	}
}

func TestImportRejectsRequestsWithoutTheFile(t *testing.T) {
	post := func(contentType, body string) *httptest.ResponseRecorder {
		importer := &fakeImporter{}
		h, _ := newImportServer(t, importer, 1<<20)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", strings.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Zero(t, importer.calls)
		return rec
	}
	tests := map[string]*httptest.ResponseRecorder{
		"json body":         post("application/json", `{"file":"x"}`),
		"no content type":   post("", "name,sku\n"),
		"form urlencoded":   post("application/x-www-form-urlencoded", "file=x"),
		"empty multipart":   post("multipart/form-data; boundary=x", "--x--\r\n"),
		"broken multipart":  post("multipart/form-data; boundary=x", "--x\r\nnot a header"),
		"missing boundary":  post("multipart/form-data", "--x--\r\n"),
		"truncated request": post("multipart/form-data; boundary=x", ""),
	}
	for name, rec := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Equal(t, "invalid_request", decodeProblem(t, rec).Code)
		})
	}

	importer := &fakeImporter{}
	h, _ := newImportServer(t, importer, 1<<20)
	rec := upload(t, h, "attachment", "products.csv", "name\n")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request", decodeProblem(t, rec).Code)
	require.Zero(t, importer.calls)
}

func TestImportBodyLargerThanTheRouteLimit(t *testing.T) {
	importer := &fakeImporter{}
	h, _ := newImportServer(t, importer, 2048)

	rec := upload(t, h, "file", "products.csv", strings.Repeat("x", 4096))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	require.Equal(t, "request_too_large", decodeProblem(t, rec).Code)

	rec = upload(t, h, "file", "products.csv", strings.Repeat("x", 1024))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestImportPassesOnlyTheBaseNameOfTheFile(t *testing.T) {
	importer := &fakeImporter{}
	h, _ := newImportServer(t, importer, 1<<20)

	upload(t, h, "file", "../../etc/products.csv", "name\n")

	require.Equal(t, "products.csv", importer.name)
}

func TestNewImportsRequiresDependencies(t *testing.T) {
	log, err := logger.New(logger.Config{Level: "info"}, io.Discard)
	require.NoError(t, err)

	_, err = httpapi.NewImports(nil, log)
	require.EqualError(t, err, "catalog: import handler requires an importer")
	_, err = httpapi.NewImports(&fakeImporter{}, nil)
	require.EqualError(t, err, "catalog: import handler requires a logger")
}
