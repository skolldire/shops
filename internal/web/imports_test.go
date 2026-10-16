package web

import (
	"bytes"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
)

const formKey = "6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"

type part struct{ name, value string }

func uploadParts(t *testing.T, h http.Handler, parts ...part) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, p := range parts {
		if p.name == "file" || p.name == "attachment" {
			w, err := form.CreateFormFile(p.name, "products.csv")
			require.NoError(t, err)
			_, err = w.Write([]byte(p.value))
			require.NoError(t, err)
			continue
		}
		require.NoError(t, form.WriteField(p.name, p.value))
	}
	require.NoError(t, form.Close())
	req := httptest.NewRequest(http.MethodPost, adminImports, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func uploadCSV(t *testing.T, h http.Handler, mode, content string) *httptest.ResponseRecorder {
	t.Helper()
	return uploadParts(t, h, part{"idempotency_key", formKey}, part{"mode", mode}, part{"file", content})
}

func TestImportFormAsksForTheModeBeforeTheFile(t *testing.T) {
	h, _ := newTestUI(t, &fakeCatalog{})

	rec := get(h, adminImports)

	require.Equal(t, http.StatusOK, rec.Code)
	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.Contains(t, body, `<form class="form-card import-form" action="/admin/imports" method="post" enctype="multipart/form-data">`)
	require.Regexp(t, `<input type="hidden" name="idempotency_key" value="[0-9a-f-]{36}">`, body)
	require.Contains(t, body, `<input id="mode-add" name="mode" type="radio" value="add" required>`)
	require.Contains(t, body, `<input id="mode-replace" name="mode" type="radio" value="replace" required>`)
	require.NotContains(t, body, "checked", "no mode is chosen for the user")
	require.Contains(t, body, "There are 50 and the file says 10: 60 remain.")
	require.Contains(t, body, "There are 50 and the file says 10: 10 remain.")
	require.Contains(t, body, `<span class="when-add">Import adding</span><span class="when-replace">Import replacing</span>`)
	require.NotContains(t, body, "allow_duplicate")
	key, mode, file := strings.Index(body, `name="idempotency_key"`), strings.Index(body, `name="mode"`), strings.Index(body, `name="file"`)
	require.True(t, key < file && mode < file, "browsers send the parts in this order, so the mode arrives before the file")
	require.NotContains(t, body, "hx-")
	require.NotContains(t, body, "8192", "configured limits are not rendered")
	require.NotEqual(t, body, get(h, adminImports).Body.String(), "every form gets its own key")
}

func report() catalog.ImportReport {
	return catalog.ImportReport{
		ID: "5a0b2c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b", Mode: catalog.ImportAdd,
		File:    catalog.ImportFile{Name: "products.csv", Bytes: 8930, SHA256: "6fc46b84"},
		Records: 97, Skipped: 2, Created: 80, Updated: 1, Unchanged: 1, Rejected: 13,
		Errors: []catalog.RowError{
			{Line: 11, SKU: "BS-021", Field: "sku", Code: "duplicate_in_file", Message: "also used on lines 56 and 89"},
			{Line: 4, SKU: "WM-042", Field: "price", Code: "invalid_format", Message: "must be a plain decimal number such as 29.99"},
		},
		Changes: []catalog.ImportChange{
			{SKU: "RS-001", StockBefore: 50, StockAfter: 60, PriceBefore: decimal.RequireFromString("29.99"), PriceAfter: decimal.RequireFromString("39.99")},
		},
	}
}

func TestAnImportIsAppliedAndReportedInTheSameRequest(t *testing.T) {
	cat := &fakeCatalog{report: report()}
	h, _ := newTestUI(t, cat)

	rec := uploadParts(t, h, part{"idempotency_key", formKey}, part{"mode", "add"}, part{"note", "ignored"}, part{"file", "name,sku\n"})

	require.Equal(t, http.StatusOK, rec.Code)
	requireSafeHTML(t, rec)
	require.Equal(t, catalog.ImportOptions{FileName: "products.csv", Mode: catalog.ImportAdd, IdempotencyKey: formKey}, cat.importOpts)
	require.Equal(t, "name,sku\n", cat.importBody)
	body := rec.Body.String()
	require.Contains(t, body, "<h2>Result for products.csv</h2>")
	require.Contains(t, body, "Added to the inventory: 80 created, 1 updated, 1 unchanged, 13 rejected and 2 empty rows skipped, out of 97 records.")
	require.Contains(t, body, `<tr><td>RS-001</td><td class="numeric">50</td><td class="numeric">60</td><td class="numeric">$29.99</td><td class="numeric">$39.99</td></tr>`)
	require.Contains(t, body, `<tr><td class="numeric">11</td><td>BS-021</td><td>sku</td><td>duplicate_in_file</td><td>also used on lines 56 and 89</td></tr>`)
	require.Contains(t, body, "8930 bytes · SHA-256 6fc46b84")
	require.NotContains(t, body, "more are not shown")
	require.Contains(t, body, `<input id="file" name="file" type="file"`, "the form stays on the page for another import")
	require.NotContains(t, body, formKey, "the next import gets a new key")
}

func TestTheModeMustArriveBeforeTheFile(t *testing.T) {
	for name, parts := range map[string][]part{
		"no mode":         {{"idempotency_key", formKey}, {"file", "name\n"}},
		"mode after file": {{"idempotency_key", formKey}, {"file", "name\n"}, {"mode", "add"}},
		"an unknown mode": {{"mode", "sum"}, {"file", "name\n"}},
	} {
		t.Run(name, func(t *testing.T) {
			cat := &fakeCatalog{}
			h, _ := newTestUI(t, cat)
			rec := uploadParts(t, h, parts...)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), "Choose whether to add to or replace the inventory before the file.")
			require.Zero(t, cat.imports, "the file is not read")
		})
	}
}

func TestARepeatedAddAsksForTheFileAgainAndAConfirmation(t *testing.T) {
	at := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	cat := &fakeCatalog{importErr: &catalog.DuplicateFileError{AppliedAt: at}}
	h, _ := newTestUI(t, cat)

	rec := uploadCSV(t, h, "add", "name\n")

	require.Equal(t, http.StatusConflict, rec.Code)
	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.Contains(t, body, "This same file was already added to the inventory on 2026-10-14 09:30 UTC, so nothing was imported.")
	require.Contains(t, body, `<input type="hidden" name="idempotency_key" value="`+formKey+`">`, "the same key is kept, since nothing was applied")
	require.Contains(t, body, `<input id="mode-add" name="mode" type="radio" value="add" required checked>`)
	require.Contains(t, body, `<input id="allow_duplicate" name="allow_duplicate" type="checkbox" value="true" required>`)
	require.Less(t, strings.Index(body, `name="allow_duplicate"`), strings.Index(body, `name="file"`))

	uploadParts(t, h, part{"idempotency_key", formKey}, part{"mode", "add"}, part{"allow_duplicate", "true"}, part{"file", "name\n"})
	require.True(t, cat.importOpts.AllowDuplicate)
}

func TestImportPagesEscapeValuesFromTheFile(t *testing.T) {
	r := report()
	r.File.Name = `<script>alert("name")</script>.csv`
	r.Changes[0].SKU = `<script>alert("sku")</script>`
	r.Errors = []catalog.RowError{{Line: 2, Message: `<script>alert("message")</script>`}}
	h, _ := newTestUI(t, &fakeCatalog{report: r})

	rec := uploadCSV(t, h, "add", "name\n")

	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.NotContains(t, body, "<script>alert")
	require.Contains(t, body, `&lt;script&gt;alert(&#34;sku&#34;)&lt;/script&gt;`)
	require.Contains(t, body, `&lt;script&gt;alert(&#34;message&#34;)&lt;/script&gt;`)
	require.Contains(t, body, `&lt;script&gt;alert(&#34;name&#34;)&lt;/script&gt;.csv`)
}

func TestImportPagesShowTheFirstTwoHundredRows(t *testing.T) {
	r := report()
	r.Errors, r.Changes = make([]catalog.RowError, 250), make([]catalog.ImportChange, 230)
	for i := range r.Errors {
		r.Errors[i] = catalog.RowError{Line: i + 2, SKU: fmt.Sprintf("BAD-%03d", i), Field: "price", Code: "invalid_format", Message: "bad"}
	}
	for i := range r.Changes {
		r.Changes[i] = catalog.ImportChange{SKU: fmt.Sprintf("OLD-%03d", i)}
	}
	h, _ := newTestUI(t, &fakeCatalog{report: r})

	body := uploadCSV(t, h, "replace", "name\n").Body.String()

	require.Equal(t, 200, strings.Count(body, "<td>invalid_format</td>"))
	require.Contains(t, body, "Showing the first 200 errors. 50 more are not shown; the API returns all of them.")
	require.Contains(t, body, "<td>OLD-199</td>")
	require.NotContains(t, body, "<td>OLD-200</td>")
	require.Contains(t, body, "Showing the first 200 existing products. 30 more are not shown; the API returns all of them.")
}

func TestImportWholeFileErrorsStayOnTheForm(t *testing.T) {
	tests := map[string]struct {
		err     error
		status  int
		message string
	}{
		"another header": {catalog.ErrInvalidHeader, 400, "The first line must be exactly name,sku,description,category,price,stock,weight_kg. Nothing was imported."},
		"malformed csv":  {&catalog.InvalidCSVError{Line: 7, Reason: `bare " in non-quoted field`}, 400, "The file is not a valid UTF-8 CSV. Line 7: bare &#34; in non-quoted field. Nothing was imported."},
		"too many rows":  {catalog.ErrTooManyRows, 400, "The file has more rows than the import limit. Nothing was imported."},
		"too large":      {catalog.ErrFileTooLarge, 413, "The file is larger than the import limit. Nothing was imported."},
		"key reused":     {catalog.ErrIdempotencyConflict, 409, "This form was already used for another file or mode."},
		"import running": {catalog.ErrImportInProgress, 409, "Another import is being applied. Nothing was imported; choose the file again in a few seconds."},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h, _ := newTestUI(t, &fakeCatalog{importErr: tt.err})

			rec := uploadCSV(t, h, "replace", "name\n")

			require.Equal(t, tt.status, rec.Code)
			requireSafeHTML(t, rec)
			body := rec.Body.String()
			require.Contains(t, body, `<p class="notice error" role="alert">`+tt.message)
			require.Contains(t, body, `<input id="file" name="file" type="file"`)
			require.NotContains(t, body, "error-status")
			require.NotContains(t, body, "import-report")
		})
	}
}

func TestImportWithoutAFileAsksForOne(t *testing.T) {
	send := func(contentType, body string) *httptest.ResponseRecorder {
		cat := &fakeCatalog{}
		h, _ := newTestUI(t, cat)
		req := httptest.NewRequest(http.MethodPost, adminImports, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Origin", testOrigin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Zero(t, cat.imports)
		return rec
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"urlencoded form": send("application/x-www-form-urlencoded", url.Values{"file": {"x"}}.Encode()),
		"empty multipart": send("multipart/form-data; boundary=x", "--x--\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), `<p class="notice error" role="alert">Choose a CSV file to import.</p>`)
		})
	}

	cat := &fakeCatalog{}
	h, _ := newTestUI(t, cat)
	rec := uploadParts(t, h, part{"mode", "add"}, part{"attachment", "name\n"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, cat.imports)
}

func TestImportHasItsOwnBodyLimit(t *testing.T) {
	h, _ := newTestUI(t, &fakeCatalog{})

	rec := uploadCSV(t, h, "replace", strings.Repeat("x", testMaxImportBodyBytes+1))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), "The file is larger than the import limit. Nothing was imported.")

	form := validForm()
	form.Set("description", strings.Repeat("x", maxFormBytes))
	rec = post(h, adminProducts, form)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "other forms keep the one megabyte limit")
}

func TestImportAcceptsUploadsLargerThanOtherForms(t *testing.T) {
	cat := &fakeCatalog{report: catalog.ImportReport{Records: 1, Created: 1}}
	log, _ := newTestLogger(t)
	ui, err := New(Deps{Catalog: cat, Ordering: &fakeOrdering{}, Users: testUsers, Log: log, MaxImportBodyBytes: 4 << 20})
	require.NoError(t, err)

	rec := uploadCSV(t, ui.Routes(), "replace", strings.Repeat("x", 2*maxFormBytes))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, cat.importBody, 2*maxFormBytes)
}

func TestImportUnexpectedFailureIsAnErrorPage(t *testing.T) {
	h, logs := newTestUI(t, &fakeCatalog{importErr: errors.New("db exploded: host=10.0.0.5")})

	rec := uploadCSV(t, h, "replace", "name\n")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), "Something went wrong")
	require.NotContains(t, rec.Body.String(), "10.0.0.5")
	require.Contains(t, logs.String(), "db exploded")
}
