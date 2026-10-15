package web

import (
	"bytes"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

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
	require.Contains(t, body, `<form class="form-card" action="/admin/imports" method="post" enctype="multipart/form-data">`)
	require.Regexp(t, `<input type="hidden" name="idempotency_key" value="[0-9a-f-]{36}">`, body)
	require.Contains(t, body, `<input id="mode-add" name="mode" type="radio" value="add" required>`)
	require.Contains(t, body, `<input id="mode-replace" name="mode" type="radio" value="replace" required>`)
	require.NotContains(t, body, "checked", "no mode is chosen for the user")
	require.Contains(t, body, "Add to the current inventory")
	require.Contains(t, body, "Replace the inventory")
	key, mode, file := strings.Index(body, `name="idempotency_key"`), strings.Index(body, `name="mode"`), strings.Index(body, `name="file"`)
	require.True(t, key < file && mode < file, "browsers send the parts in this order, so the mode arrives before the file")
	require.NotContains(t, body, "hx-")
	require.NotContains(t, body, "8192", "configured limits are not rendered")
	require.NotEqual(t, body, get(h, adminImports).Body.String(), "every form gets its own key")
}

func TestUploadingAFileCreatesAPreviewAndShowsIt(t *testing.T) {
	cat := &fakeCatalog{}
	h, _ := newTestUI(t, cat)

	rec := uploadCSV(t, h, "add", "name,sku\n")

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, adminImports+"/"+importID, rec.Header().Get("Location"))
	require.Equal(t, catalog.ImportOptions{FileName: "products.csv", Mode: catalog.ImportAdd, IdempotencyKey: formKey}, cat.importOpts)
	require.Equal(t, "name,sku\n", cat.importBody)
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

func preview(status catalog.ImportStatus) catalog.ImportPreview {
	return catalog.ImportPreview{
		ID: importID, Status: status, Mode: catalog.ImportReplace,
		File: catalog.ImportFile{Name: "stock.csv", Bytes: 120, SHA256: "6fc46b84"},
		New:  1, Existing: 2,
		Report: catalog.ImportReport{Records: 5, Rejected: 2, Errors: []catalog.RowError{
			{Line: 3, SKU: "HELD-1", Field: "stock", Code: "below_reserved", Message: "must be at least the units reserved by pending purchases"},
			{Line: 4, SKU: "BAD-1", Field: "price", Code: "invalid_format", Message: "must be a plain decimal number such as 29.99"},
		}},
		Rows: []catalog.PreviewRow{{SKU: "OLD-1", StockBefore: 10, StockAfter: 3, Reserved: 1}, {SKU: "HELD-1", StockBefore: 5, StockAfter: 1, Reserved: 2}},
	}
}

func TestThePreviewShowsTheChangesWithConfirmAndCancel(t *testing.T) {
	h, _ := newTestUI(t, &fakeCatalog{preview: preview(catalog.ImportPreviewing)})

	rec := get(h, adminImports+"/"+importID)

	require.Equal(t, http.StatusOK, rec.Code)
	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.Contains(t, body, "Replace the inventory · 120 bytes · SHA-256 6fc46b84")
	require.Contains(t, body, "1 new products, 2 existing products and 2 rejected rows, out of 5 records.")
	require.Contains(t, body, `<tr><td>OLD-1</td><td class="numeric">10</td><td class="numeric">3</td><td class="numeric">1</td></tr>`)
	require.Contains(t, body, `<td>HELD-1</td><td>stock</td><td>below_reserved</td>`)
	require.Contains(t, body, `<form action="/admin/imports/`+importID+`/confirm" method="post">`)
	require.Contains(t, body, `<form action="/admin/imports/`+importID+`/cancel" method="post" class="actions">`)
	require.NotContains(t, body, "allow_duplicate")
	require.NotContains(t, body, "import-report")
}

func TestARepeatedAddNeedsTheConfirmationBox(t *testing.T) {
	p := preview(catalog.ImportPreviewing)
	p.Mode = catalog.ImportAdd
	at := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	p.DuplicateOf = &at
	cat := &fakeCatalog{preview: p, confirmErr: catalog.ErrDuplicateFile}
	h, _ := newTestUI(t, cat)

	body := get(h, adminImports+"/"+importID).Body.String()
	require.Contains(t, body, "This same file was already added to the inventory on 2026-10-14 09:30 UTC.")
	require.Contains(t, body, `<input id="allow_duplicate" name="allow_duplicate" type="checkbox" value="true" required>`)

	rec := post(h, adminImports+"/"+importID+"/confirm", url.Values{})
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "Tick the confirmation to add the same file again.")
	post(h, adminImports+"/"+importID+"/confirm", url.Values{"allow_duplicate": {"true"}})
	require.Equal(t, []bool{false, true}, cat.confirmed)
}

func TestConfirmingAndCancellingRedirectToTheImport(t *testing.T) {
	cat := &fakeCatalog{preview: preview(catalog.ImportPreviewing)}
	h, _ := newTestUI(t, cat)

	rec := post(h, adminImports+"/"+importID+"/confirm", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, adminImports+"/"+importID, rec.Header().Get("Location"))
	rec = post(h, adminImports+"/"+importID+"/cancel", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, 1, cat.cancelled)

	require.Equal(t, http.StatusNotFound, get(h, adminImports+"/other").Code)
	require.Equal(t, http.StatusNotFound, post(h, adminImports+"/other/confirm", url.Values{}).Code)
}

func TestConfirmationProblemsStayOnTheImport(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		path    string
		status  int
		message string
	}{
		"expired":          {catalog.ErrPreviewExpired, "/confirm", http.StatusGone, "This preview expired. Upload the file again to import it."},
		"cancelled":        {catalog.ErrImportCancelled, "/confirm", http.StatusConflict, "This import was cancelled."},
		"not cancellable":  {catalog.ErrImportNotCancellable, "/cancel", http.StatusConflict, "cannot be cancelled"},
		"unexpected error": {errors.New("db exploded: host=10.0.0.5"), "/confirm", http.StatusInternalServerError, "Something went wrong"},
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := newTestUI(t, &fakeCatalog{preview: preview(catalog.ImportCancelled), confirmErr: tc.err})
			rec := post(h, adminImports+"/"+importID+tc.path, url.Values{})
			require.Equal(t, tc.status, rec.Code)
			require.Contains(t, rec.Body.String(), tc.message)
			require.NotContains(t, rec.Body.String(), "10.0.0.5")
		})
	}
}

func TestEachImportStatusHasItsPage(t *testing.T) {
	for status, want := range map[catalog.ImportStatus]string{
		catalog.ImportApplying:  "This import stopped while it was being applied.",
		catalog.ImportCancelled: "This import was cancelled. Nothing was changed.",
		catalog.ImportApplied:   `<section id="import-report">`,
	} {
		t.Run(string(status), func(t *testing.T) {
			p := preview(status)
			p.CancelReason = "user"
			h, _ := newTestUI(t, &fakeCatalog{preview: p})
			body := get(h, adminImports+"/"+importID).Body.String()
			require.Contains(t, body, want)
			require.NotContains(t, body, "/cancel\"", "only a preview can be cancelled")
		})
	}
	expired := preview(catalog.ImportCancelled)
	expired.CancelReason = "expired"
	h, _ := newTestUI(t, &fakeCatalog{preview: expired})
	require.Contains(t, get(h, adminImports+"/"+importID).Body.String(), "This preview expired before it was confirmed.")

	changed := preview(catalog.ImportApplied)
	changed.Report.PreviewChanged = true
	h, _ = newTestUI(t, &fakeCatalog{preview: changed})
	require.Contains(t, get(h, adminImports+"/"+importID).Body.String(), "The stock changed between the preview and the confirmation.")
}

func TestImportPagesEscapeValuesFromTheFile(t *testing.T) {
	p := preview(catalog.ImportPreviewing)
	p.File.Name = `<script>alert("name")</script>.csv`
	p.Rows = []catalog.PreviewRow{{SKU: `<script>alert("sku")</script>`}}
	p.Report.Errors = []catalog.RowError{{Line: 2, Message: `<script>alert("message")</script>`}}
	h, _ := newTestUI(t, &fakeCatalog{preview: p})

	rec := get(h, adminImports+"/"+importID)

	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.NotContains(t, body, "<script>alert")
	require.Contains(t, body, `&lt;script&gt;alert(&#34;sku&#34;)&lt;/script&gt;`)
	require.Contains(t, body, `&lt;script&gt;alert(&#34;message&#34;)&lt;/script&gt;`)
}

func TestImportPagesShowTheFirstTwoHundredRows(t *testing.T) {
	p := preview(catalog.ImportPreviewing)
	p.Report.Errors, p.Rows = make([]catalog.RowError, 250), make([]catalog.PreviewRow, 230)
	for i := range p.Report.Errors {
		p.Report.Errors[i] = catalog.RowError{Line: i + 2, SKU: fmt.Sprintf("BAD-%03d", i), Field: "price", Code: "invalid_format", Message: "bad"}
	}
	for i := range p.Rows {
		p.Rows[i] = catalog.PreviewRow{SKU: fmt.Sprintf("OLD-%03d", i)}
	}
	h, _ := newTestUI(t, &fakeCatalog{preview: p})

	body := get(h, adminImports+"/"+importID).Body.String()

	require.Equal(t, 200, strings.Count(body, "<td>invalid_format</td>"))
	require.Contains(t, body, "Showing the first 200 errors. 50 more are not shown; the API returns all of them.")
	require.Equal(t, 200, len(regexp.MustCompile(`<td>OLD-\d{3}</td>`).FindAllString(body, -1)))
	require.Contains(t, body, "Showing the first 200 existing products. 30 more are not shown.")
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
		"previews":       {catalog.ErrTooManyPreviews, 429, "Too many imports are waiting for a confirmation."},
		"key reused":     {catalog.ErrIdempotencyConflict, 409, "This form was already used for another file."},
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
	cat := &fakeCatalog{}
	log, _ := newTestLogger(t)
	ui, err := New(Deps{Catalog: cat, Ordering: &fakeOrdering{}, Users: testUsers, Log: log, MaxImportBodyBytes: 4 << 20})
	require.NoError(t, err)

	rec := uploadCSV(t, ui.Routes(), "replace", strings.Repeat("x", 2*maxFormBytes))

	require.Equal(t, http.StatusSeeOther, rec.Code)
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
