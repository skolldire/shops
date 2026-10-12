package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func multipartFile(t *testing.T, content string) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "products.csv")
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, form.Close())
	return &body, form.FormDataContentType()
}

func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	return p.Code
}

func TestImportsHaveTheirOwnBodyLimit(t *testing.T) {
	a := testApp(t)
	maxBytes := shippedConfig(t).Import.MaxBytes
	post := func(path, contentType string, body *bytes.Buffer) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, body)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		a.api.ServeHTTP(rec, req)
		return rec
	}

	t.Run("the rest of the api keeps one megabyte", func(t *testing.T) {
		big := bytes.NewBufferString(`{"name":"` + strings.Repeat("x", maxRequestBytes) + `"}`)

		rec := post("/api/v1/products", "application/json", big)

		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		require.Equal(t, "request_too_large", problemCode(t, rec))
	})

	t.Run("an import larger than one megabyte is read", func(t *testing.T) {
		body, contentType := multipartFile(t, "sku,name\n"+strings.Repeat("x", 2*maxRequestBytes))

		rec := post("/api/v1/imports", contentType, body)

		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Equal(t, "invalid_header", problemCode(t, rec))
	})

	t.Run("an import of exactly the limit is read", func(t *testing.T) {
		body, contentType := multipartFile(t, "sku,name\n"+strings.Repeat("x", int(maxBytes)-len("sku,name\n")))

		rec := post("/api/v1/imports", contentType, body)

		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Equal(t, "invalid_header", problemCode(t, rec))
	})

	t.Run("an import one byte over the limit is rejected", func(t *testing.T) {
		header := "name,sku,description,category,price,stock,weight_kg\n"
		body, contentType := multipartFile(t, header+strings.Repeat(" ", int(maxBytes)-len(header)+1))

		rec := post("/api/v1/imports", contentType, body)

		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		require.Equal(t, "request_too_large", problemCode(t, rec))
	})

	t.Run("an eleven megabyte import is rejected", func(t *testing.T) {
		header := "name,sku,description,category,price,stock,weight_kg\n"
		body, contentType := multipartFile(t, header+strings.Repeat(" ", 11<<20))

		rec := post("/api/v1/imports", contentType, body)

		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		require.Equal(t, "request_too_large", problemCode(t, rec))
	})
}
