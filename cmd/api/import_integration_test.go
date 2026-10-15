//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/lifecycle"
	"github.com/skolldire/shops/internal/platform/logger"
)

const (
	officialCSV    = "../../testdata/products.csv"
	officialSHA256 = "6fc46b84badb1ac37b8b19a2d0741aa7728d6c93a9fe949b19f676892dc8fd24"
	importHeader   = "name,sku,description,category,price,stock,weight_kg\n"
)

type reportJSON struct {
	File struct {
		Name   string `json:"name"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"file"`
	Records   int `json:"records"`
	Skipped   int `json:"skipped"`
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Rejected  int `json:"rejected"`
	Errors    []struct {
		Line    int    `json:"line"`
		SKU     string `json:"sku"`
		Field   string `json:"field"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

func (r reportJSON) rejections() []string {
	out := make([]string, len(r.Errors))
	for i, e := range r.Errors {
		out[i] = fmt.Sprintf("%d %s %s %s", e.Line, e.SKU, e.Field, e.Code)
	}
	return out
}

func (c client) upload(path, name string, content []byte) (*http.Response, string) {
	c.t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", name)
	require.NoError(c.t, err)
	_, err = part.Write(content)
	require.NoError(c.t, err)
	require.NoError(c.t, form.Close())
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodPost, c.base+path, &body)
	require.NoError(c.t, err)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Origin", c.base)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return resp, string(b)
}

func (c client) importReport(content []byte) reportJSON {
	c.t.Helper()
	resp, body := c.upload("/api/v1/imports", "products.csv", content)
	require.Equal(c.t, http.StatusOK, resp.StatusCode, body)
	var report reportJSON
	require.NoError(c.t, json.Unmarshal([]byte(body), &report), body)
	return report
}

func (c client) uploadImport(mode string, content []byte) (*http.Response, string) {
	c.t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	require.NoError(c.t, form.WriteField("idempotency_key", uuid.NewString()))
	require.NoError(c.t, form.WriteField("mode", mode))
	part, err := form.CreateFormFile("file", "products.csv")
	require.NoError(c.t, err)
	_, err = part.Write(content)
	require.NoError(c.t, err)
	require.NoError(c.t, form.Close())
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodPost, c.base+"/admin/imports", &body)
	require.NoError(c.t, err)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Origin", c.base)
	return c.send(req)
}

func (c client) send(req *http.Request) (*http.Response, string) {
	c.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return resp, string(out)
}

func (c client) products() (total, active, maxVersion int) {
	c.t.Helper()
	require.NoError(c.t, c.pool.QueryRow(c.t.Context(),
		`SELECT count(*), count(*) FILTER (WHERE status = 'ACTIVE'), coalesce(max(version), 0) FROM products`).
		Scan(&total, &active, &maxVersion))
	return total, active, maxVersion
}

var officialRejections = []string{
	"2 RS-001 sku duplicate_in_file",
	"4 WM-042 price invalid_format",
	"7 YM-015 price invalid_format",
	"11 BS-021 sku duplicate_in_file",
	"16 DL-007 stock out_of_range",
	"25 HD-099 name required",
	"36 RS-001 sku duplicate_in_file",
	"41 WS-001 name required",
	"47 MB-001 price not_positive",
	"50 GK-088 weight_kg required",
	"52 GC-025 category required",
	"56 BS-021 sku duplicate_in_file",
	"89 BS-021 sku duplicate_in_file",
}

func TestOfficialCSVImport(t *testing.T) {
	c := startApp(t)
	official, err := os.ReadFile(officialCSV)
	require.NoError(t, err)

	first := c.importReport(official)

	require.Equal(t, "products.csv", first.File.Name)
	require.EqualValues(t, len(official), first.File.Bytes)
	require.Equal(t, officialSHA256, first.File.SHA256)
	require.Equal(t, []int{97, 2, 82, 0, 0, 13}, []int{first.Records, first.Skipped, first.Created, first.Updated, first.Unchanged, first.Rejected})
	require.Equal(t, officialRejections, first.rejections())
	messages := map[int]string{}
	for _, e := range first.Errors {
		if e.Code == "duplicate_in_file" {
			messages[e.Line] = e.Message
		}
	}
	require.Equal(t, map[int]string{
		2: "also used on line 36", 36: "also used on line 2",
		11: "also used on lines 56 and 89", 56: "also used on lines 11 and 89", 89: "also used on lines 11 and 56",
	}, messages)
	total, active, maxVersion := c.products()
	require.Equal(t, []int{82, 82, 1}, []int{total, active, maxVersion})

	second := c.importReport(official)

	require.Equal(t, []int{97, 2, 0, 0, 82, 13}, []int{second.Records, second.Skipped, second.Created, second.Updated, second.Unchanged, second.Rejected})
	require.Equal(t, officialRejections, second.rejections())
	_, _, maxVersion = c.products()
	require.Equal(t, 1, maxVersion, "importing the same file again does not touch any version")

	resp, body := c.do(http.MethodGet, "/api/v1/products?q=CB-010", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var page struct {
		Items []struct {
			ID   string `json:"id"`
			SKU  string `json:"sku"`
			Name string `json:"name"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, "Organic Coffee Beans", page.Items[0].Name)
	resp, _ = c.do(http.MethodDelete, "/api/v1/products/"+page.Items[0].ID, "", "If-Match", `"1"`)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	third := c.importReport(official)

	require.Equal(t, []int{97, 2, 0, 0, 81, 14}, []int{third.Records, third.Skipped, third.Created, third.Updated, third.Unchanged, third.Rejected})
	require.Contains(t, third.rejections(), "3 CB-010 sku sku_deleted")
	require.Len(t, third.Errors, 14)
	total, active, _ = c.products()
	require.Equal(t, []int{82, 81}, []int{total, active}, "a deleted sku is not revived")

	resp, body = c.do(http.MethodGet, "/?q=tent", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "Camping Tent")
	resp, body = c.do(http.MethodGet, "/?q=XS-001", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "&lt;script&gt;alert(&#39;xss&#39;)&lt;/script&gt;")
	require.NotContains(t, body, "<script>alert(")

	resp, body = c.uploadImport("replace", official)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "Replaced the inventory: 0 created, 0 updated, 81 unchanged, 14 rejected and 2 empty rows skipped, out of 97 records.")
	require.Contains(t, body, `<tr><td class="numeric">89</td><td>BS-021</td><td>sku</td><td>duplicate_in_file</td><td>also used on lines 11 and 56</td></tr>`)
	require.Contains(t, body, `<tr><td class="numeric">3</td><td>CB-010</td><td>sku</td><td>sku_deleted</td>`)
	require.Contains(t, body, officialSHA256)

	rec := httptest.NewRecorder()
	c.app.metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	metrics := rec.Body.String()
	for _, want := range []string{
		`import_rows_total{outcome="created"} 82`,
		`import_rows_total{outcome="updated"} 0`,
		`import_rows_total{outcome="unchanged"} 244`,
		`import_rows_total{outcome="skipped"} 8`,
		`import_rows_total{outcome="rejected"} 54`,
		`import_duration_seconds_count 4`,
	} {
		require.Contains(t, metrics, want)
	}
}

func TestImportLimitsLeaveTheDatabaseUntouched(t *testing.T) {
	c := startApp(t)
	c.importReport([]byte(importHeader + "Shoes,RS-1,,Sports,9.99,1,0.3\n"))
	before := func() string {
		var state string
		require.NoError(t, c.pool.QueryRow(t.Context(),
			`SELECT count(*)::text || ':' || max(version)::text || ':' || max(updated_at)::text FROM products`).Scan(&state))
		return state
	}
	state := before()
	row := "Shoes v2,RS-1,,Sports,19.99,5,0.3\n"

	resp, body := c.upload("/api/v1/imports", "big.csv", []byte(importHeader+row+strings.Repeat(" ", 11<<20)))
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, body)
	code, _ := errorCodes(t, body)
	require.Equal(t, "request_too_large", code)
	require.Equal(t, state, before())

	resp, body = c.upload("/api/v1/imports", "long.csv", []byte(importHeader+row+strings.Repeat("New,N-1,,A,1,1,1\n", c.cfg.Import.MaxRows)))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	code, _ = errorCodes(t, body)
	require.Equal(t, "too_many_rows", code)
	require.Equal(t, state, before())

	exact := c.importReport([]byte(importHeader + strings.Repeat(",,,,,,\n", c.cfg.Import.MaxRows)))
	require.Equal(t, c.cfg.Import.MaxRows, exact.Skipped)

	for name, tt := range map[string]struct {
		content string
		code    string
	}{
		"header": {"sku,name\n" + row, "invalid_header"},
		"quotes": {importHeader + "Shoes,\"RS-1,,Sports,19.99,5,0.3\n", "invalid_csv"},
		"utf-8":  {importHeader + "Sho\xffes,RS-1,,Sports,19.99,5,0.3\n", "invalid_csv"},
	} {
		resp, body = c.upload("/api/v1/imports", "bad.csv", []byte(tt.content))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, name)
		code, _ = errorCodes(t, body)
		require.Equal(t, tt.code, code, name)
		require.Equal(t, state, before(), name)
	}
}

func TestDemoSeed(t *testing.T) {
	c := startApp(t)
	ctx := t.Context()

	require.NoError(t, seedDemo(ctx, c.log, c.app.catalog, officialCSV))
	total, active, _ := c.products()
	require.Equal(t, []int{82, 82}, []int{total, active}, "an empty catalog is seeded with the valid rows")

	_, err := c.pool.Exec(ctx, `UPDATE products SET status = 'DELETED', deleted_at = now() WHERE sku = 'CB-010'`)
	require.NoError(t, err)
	_, err = c.pool.Exec(ctx, `UPDATE products SET stock = 1 WHERE sku = 'CT-005'`)
	require.NoError(t, err)

	require.NoError(t, seedDemo(ctx, c.log, c.app.catalog, officialCSV))
	total, active, _ = c.products()
	require.Equal(t, []int{82, 81}, []int{total, active}, "a catalog with products is left alone")
	var stock int
	require.NoError(t, c.pool.QueryRow(ctx, `SELECT stock FROM products WHERE sku = 'CT-005'`).Scan(&stock))
	require.Equal(t, 1, stock)

	_, err = c.pool.Exec(ctx, `UPDATE products SET status = 'DELETED', deleted_at = now()`)
	require.NoError(t, err)
	require.NoError(t, seedDemo(ctx, c.log, c.app.catalog, officialCSV))
	_, active, _ = c.products()
	require.Zero(t, active, "deleted products still count, so nothing is revived")

	missing := filepath.Join(t.TempDir(), "missing.csv")
	err = seedDemo(ctx, c.log, c.app.catalog, missing)
	require.EqualError(t, err, "demo seed: open seed file: no such file or directory")
	require.NotContains(t, err.Error(), missing)
}

func TestStartSeedsBeforeServingAndFailsWithoutTheFile(t *testing.T) {
	c := startApp(t)
	cfg := c.cfg
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Metrics.Addr = "127.0.0.1:0"
	cfg.Demo.Seed = true
	var logs syncBuffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)

	t.Run("a missing seed file stops the startup", func(t *testing.T) {
		lc, err := lifecycle.New(log)
		require.NoError(t, err)
		failing := cfg
		failing.Demo.SeedFile = filepath.Join(t.TempDir(), "missing.csv")

		err = start(t.Context(), failing, log, lc)

		require.EqualError(t, err, "demo seed: open seed file: no such file or directory")
		require.NoError(t, lc.Close(context.Background()))
		total, _, _ := c.products()
		require.Zero(t, total)
	})

	t.Run("the catalog is seeded and the summary is logged", func(t *testing.T) {
		lc, err := lifecycle.New(log)
		require.NoError(t, err)
		seeded := cfg
		seeded.Demo.SeedFile = officialCSV
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- start(ctx, seeded, log, lc) }()

		require.Eventually(t, func() bool { return strings.Contains(logs.String(), "server listening") }, 20*time.Second, 20*time.Millisecond)
		total, _, _ := c.products()
		require.Equal(t, 82, total)
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("the app did not stop")
		}
		require.NoError(t, lc.Close(context.Background()))
		require.Contains(t, logs.String(), `"message":"demo seed imported"`)
		require.Contains(t, logs.String(), `"created":82`)
		require.Contains(t, logs.String(), `"rejected":13`)
		require.Contains(t, logs.String(), `"skipped":2`)
		require.NotContains(t, logs.String(), "products.csv", "the configured path is never logged")
	})
}
