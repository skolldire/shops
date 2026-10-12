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

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
)

func uploadCSV(t *testing.T, h http.Handler, field, name, content string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile(field, name)
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, form.Close())
	req := httptest.NewRequest(http.MethodPost, adminImports, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestImportFormIsAPlainMultipartForm(t *testing.T) {
	h, _ := newTestUI(t, &fakeCatalog{})

	rec := get(h, adminImports)

	require.Equal(t, http.StatusOK, rec.Code)
	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.Contains(t, body, `<form class="form-card" action="/admin/imports" method="post" enctype="multipart/form-data">`)
	require.Contains(t, body, `<input id="file" name="file" type="file"`)
	require.Contains(t, body, "name,sku,description,category,price,stock,weight_kg")
	require.NotContains(t, body, "import-report")
	require.NotContains(t, body, "hx-")
	require.NotContains(t, body, "8192", "configured limits are not rendered")
}

func TestImportShowsTheSummaryAndTheRejectedRows(t *testing.T) {
	cat := &fakeCatalog{report: catalog.ImportReport{
		File:    catalog.ImportFile{Name: "products.csv", Bytes: 8930, SHA256: "6fc46b84"},
		Records: 97, Skipped: 2, Created: 80, Updated: 1, Unchanged: 1, Rejected: 13,
		Errors: []catalog.RowError{
			{Line: 11, SKU: "BS-021", Field: "sku", Code: "duplicate_in_file", Message: "also used on lines 56 and 89"},
			{Line: 4, SKU: "WM-042", Field: "price", Code: "invalid_format", Message: "must be a plain decimal number such as 29.99"},
		},
	}}
	h, _ := newTestUI(t, cat)

	rec := uploadCSV(t, h, "file", "products.csv", "name,sku\n")

	require.Equal(t, http.StatusOK, rec.Code)
	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.Equal(t, "products.csv", cat.importName)
	require.Equal(t, "name,sku\n", cat.importBody)
	require.Contains(t, body, "<h2>Result for products.csv</h2>")
	require.Contains(t, body, "80 created, 1 updated, 1 unchanged, 13 rejected and 2 empty rows skipped, out of 97 records.")
	require.Contains(t, body, `<tr><td class="numeric">97</td><td class="numeric">80</td><td class="numeric">1</td><td class="numeric">1</td><td class="numeric">13</td><td class="numeric">2</td></tr>`)
	require.Contains(t, body, "8930 bytes · SHA-256 6fc46b84")
	require.Contains(t, body, `<tr><td class="numeric">11</td><td>BS-021</td><td>sku</td><td>duplicate_in_file</td><td>also used on lines 56 and 89</td></tr>`)
	require.Contains(t, body, `<tr><td class="numeric">4</td><td>WM-042</td><td>price</td><td>invalid_format</td><td>must be a plain decimal number such as 29.99</td></tr>`)
	require.NotContains(t, body, "more are not shown")
	require.Contains(t, body, `<input id="file" name="file" type="file"`, "the form stays on the page for another upload")
}

func TestImportWithoutRejectedRowsHasNoErrorTable(t *testing.T) {
	h, _ := newTestUI(t, &fakeCatalog{report: catalog.ImportReport{Records: 1, Created: 1}})

	body := uploadCSV(t, h, "file", "products.csv", "name\n").Body.String()

	require.Contains(t, body, "import-report")
	require.NotContains(t, body, "import-errors")
}

func TestImportEscapesValuesFromTheFile(t *testing.T) {
	cat := &fakeCatalog{report: catalog.ImportReport{
		File:     catalog.ImportFile{Name: `<script>alert("name")</script>.csv`},
		Rejected: 1,
		Errors: []catalog.RowError{
			{Line: 2, SKU: `<script>alert("sku")</script>`, Field: "sku", Code: "invalid_format", Message: `<script>alert("message")</script>`},
		},
	}}
	h, _ := newTestUI(t, cat)

	rec := uploadCSV(t, h, "file", "x.csv", "name\n")

	requireSafeHTML(t, rec)
	body := rec.Body.String()
	require.NotContains(t, body, "<script>alert")
	require.Contains(t, body, `&lt;script&gt;alert(&#34;sku&#34;)&lt;/script&gt;`)
	require.Contains(t, body, `&lt;script&gt;alert(&#34;message&#34;)&lt;/script&gt;`)
	require.Contains(t, body, `&lt;script&gt;alert(&#34;name&#34;)&lt;/script&gt;.csv`)
}

func TestImportShowsTheFirstTwoHundredErrors(t *testing.T) {
	errs := make([]catalog.RowError, 250)
	for i := range errs {
		errs[i] = catalog.RowError{Line: i + 2, SKU: fmt.Sprintf("SKU-%03d", i), Field: "price", Code: "invalid_format", Message: "bad"}
	}
	h, _ := newTestUI(t, &fakeCatalog{report: catalog.ImportReport{Rejected: 250, Errors: errs}})

	body := uploadCSV(t, h, "file", "products.csv", "name\n").Body.String()

	require.Equal(t, 200, strings.Count(body, "<td>invalid_format</td>"))
	require.Contains(t, body, "<td>SKU-199</td>")
	require.NotContains(t, body, "<td>SKU-200</td>")
	require.Contains(t, body, "Showing the first 200 errors. 50 more are not shown; the API returns all of them.")
}

func TestImportWholeFileErrorsStayOnThePage(t *testing.T) {
	tests := map[string]struct {
		err     error
		status  int
		message string
	}{
		"another header": {catalog.ErrInvalidHeader, 400, "The first line must be exactly name,sku,description,category,price,stock,weight_kg. Nothing was imported."},
		"malformed csv":  {&catalog.InvalidCSVError{Line: 7, Reason: `bare " in non-quoted field`}, 400, "The file is not a valid UTF-8 CSV. Line 7: bare &#34; in non-quoted field. Nothing was imported."},
		"too many rows":  {catalog.ErrTooManyRows, 400, "The file has more rows than the import limit. Nothing was imported."},
		"too large":      {catalog.ErrFileTooLarge, 413, "The file is larger than the import limit. Nothing was imported."},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h, _ := newTestUI(t, &fakeCatalog{importErr: tt.err})

			rec := uploadCSV(t, h, "file", "products.csv", "name\n")

			require.Equal(t, tt.status, rec.Code)
			requireSafeHTML(t, rec)
			body := rec.Body.String()
			require.Contains(t, body, `<p class="notice error" role="alert">`+tt.message+`</p>`)
			require.Contains(t, body, `<input id="file" name="file" type="file"`)
			require.NotContains(t, body, "error-status")
			require.NotContains(t, body, "import-report")
		})
	}
}

func TestImportWithoutAFileAsksForOne(t *testing.T) {
	post := func(contentType, body string) *httptest.ResponseRecorder {
		cat := &fakeCatalog{}
		h, _ := newTestUI(t, cat)
		req := httptest.NewRequest(http.MethodPost, adminImports, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Zero(t, cat.imports)
		return rec
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"urlencoded form": post("application/x-www-form-urlencoded", url.Values{"file": {"x"}}.Encode()),
		"empty multipart": post("multipart/form-data; boundary=x", "--x--\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), `<p class="notice error" role="alert">Choose a CSV file to import.</p>`)
		})
	}

	cat := &fakeCatalog{}
	h, _ := newTestUI(t, cat)
	rec := uploadCSV(t, h, "attachment", "products.csv", "name\n")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, cat.imports)
}

func TestImportHasItsOwnBodyLimit(t *testing.T) {
	cat := &fakeCatalog{report: catalog.ImportReport{Records: 1, Created: 1}}
	h, _ := newTestUI(t, cat)

	rec := uploadCSV(t, h, "file", "products.csv", strings.Repeat("x", testMaxImportBodyBytes+1))

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
	ui, err := New(Deps{Catalog: cat, Log: log, MaxImportBodyBytes: 4 << 20})
	require.NoError(t, err)

	rec := uploadCSV(t, ui.Routes(), "file", "products.csv", strings.Repeat("x", 2*maxFormBytes))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, cat.importBody, 2*maxFormBytes)
}

func TestImportUnexpectedFailureIsAnErrorPage(t *testing.T) {
	h, logs := newTestUI(t, &fakeCatalog{importErr: errors.New("db exploded: host=10.0.0.5")})

	rec := uploadCSV(t, h, "file", "products.csv", "name\n")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), "Something went wrong")
	require.NotContains(t, rec.Body.String(), "10.0.0.5")
	require.Contains(t, logs.String(), "db exploded")
}
