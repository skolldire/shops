//go:build integration

package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

var importKey = regexp.MustCompile(`name="idempotency_key" value="([0-9a-f-]{36})"`)

func (c client) submitImport(fields [][2]string, content string) (*http.Response, string) {
	c.t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, f := range fields {
		if f[0] == "file" {
			part, err := form.CreateFormFile("file", "delivery.csv")
			require.NoError(c.t, err)
			_, err = part.Write([]byte(content))
			require.NoError(c.t, err)
			continue
		}
		require.NoError(c.t, form.WriteField(f[0], f[1]))
	}
	require.NoError(c.t, form.Close())
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodPost, c.base+"/admin/imports", &body)
	require.NoError(c.t, err)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Origin", c.base)
	return c.send(req)
}

func (c client) idOf(sku string) string {
	c.t.Helper()
	var id string
	require.NoError(c.t, c.pool.QueryRow(c.t.Context(), `SELECT id::text FROM products WHERE sku = $1`, sku).Scan(&id))
	return id
}

func TestImportFromTheUIEndToEnd(t *testing.T) {
	c := startApp(t)
	c.product("TENT-1", "29.99", 50)
	delivery := importHeader + "Tent,TENT-1,,Camping,39.99,10,3\nLamp,LAMP-1,,Camping,9.00,2,1\n"

	resp, page := c.do(http.MethodGet, "/admin/imports", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	key := importKey.FindStringSubmatch(page)[1]
	fields := [][2]string{{"idempotency_key", key}, {"mode", "add"}, {"file", ""}}

	resp, body := c.submitImport(fields, delivery)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, "Added to the inventory: 1 created, 1 updated, 0 unchanged, 0 rejected")
	require.Contains(t, body, `<tr><td>TENT-1</td><td class="numeric">50</td><td class="numeric">60</td><td class="numeric">$29.99</td><td class="numeric">$39.99</td></tr>`)
	require.NotContains(t, body, "<td>LAMP-1</td>", "a new product is counted, not listed as a change")
	require.Equal(t, 60, c.stockOf(c.idOf("TENT-1")))

	resp, again := c.submitImport(fields, delivery)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, again, `<td class="numeric">50</td><td class="numeric">60</td>`, "a resubmitted form answers the original report")
	require.Equal(t, 60, c.stockOf(c.idOf("TENT-1")), "and adds nothing")

	otherKey := importKey.FindStringSubmatch(again)[1]
	resp, body = c.submitImport([][2]string{{"idempotency_key", otherKey}, {"mode", "add"}, {"file", ""}}, delivery)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Contains(t, body, "This same file was already added to the inventory on")
	require.Contains(t, body, `name="allow_duplicate"`)
	resp, body = c.submitImport([][2]string{{"idempotency_key", otherKey}, {"mode", "add"}, {"allow_duplicate", "true"}, {"file", ""}}, delivery)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, `<td class="numeric">60</td><td class="numeric">70</td>`)

	resp, body = c.submitImport([][2]string{{"idempotency_key", otherKey}, {"file", ""}, {"mode", "replace"}}, delivery)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, body, "Choose whether to add to or replace the inventory before the file.")
	require.Equal(t, 70, c.stockOf(c.idOf("TENT-1")))
}
