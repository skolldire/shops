//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/database/dbtest"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

type client struct {
	t    *testing.T
	base string
	pool *pgxpool.Pool
}

func (c client) do(method, path, body string, headers ...string) (*http.Response, string) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+path, strings.NewReader(body))
	require.NoError(c.t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return resp, string(b)
}

func startApp(t *testing.T) client {
	t.Helper()
	pool, dbCfg := dbtest.Start(t)
	cfg := shippedConfig(t)
	cfg.Database = dbCfg
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	tel, err := telemetry.New(t.Context(), cfg.Telemetry, log)
	require.NoError(t, err)
	a, err := buildApp(cfg, log, pool, tel)
	require.NoError(t, err)
	srv := httptest.NewServer(a.api)
	t.Cleanup(srv.Close)
	return client{t: t, base: srv.URL, pool: pool}
}

func errorCodes(t *testing.T, body string) (string, map[string]string) {
	t.Helper()
	var p struct {
		Code   string `json:"code"`
		Errors []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &p), body)
	fields := map[string]string{}
	for _, fe := range p.Errors {
		fields[fe.Field] = fe.Code
	}
	return p.Code, fields
}

func TestProductLifecycleOverHTTP(t *testing.T) {
	c := startApp(t)
	product := `{"sku":"rs-001","name":"Running Shoes","description":"Light","category":"Sports","price":"29.99","stock":10,"weight_kg":"0.850"}`

	resp, body := c.do(http.MethodPost, "/api/v1/products", product)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	require.Equal(t, `"1"`, resp.Header.Get("ETag"))
	location := resp.Header.Get("Location")
	require.True(t, strings.HasPrefix(location, "/api/v1/products/"), location)
	var created map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, "RS-001", created["sku"])
	require.Equal(t, "29.99", created["price"])
	require.Equal(t, "0.850", created["weight_kg"])

	resp, _ = c.do(http.MethodGet, location, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, `"1"`, resp.Header.Get("ETag"))

	resp, body = c.do(http.MethodGet, "/api/v1/products?q=running&category=sports&in_stock=true", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, `"total":1`)

	resp, body = c.do(http.MethodPatch, location, `{"stock":7,"price":"27.50"}`, "If-Match", `"1"`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, `"2"`, resp.Header.Get("ETag"))
	require.Contains(t, body, `"stock":7`)
	require.Contains(t, body, `"price":"27.50"`)

	resp, body = c.do(http.MethodPatch, location, `{"stock":100}`, "If-Match", `"1"`)
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
	code, _ := errorCodes(t, body)
	require.Equal(t, "version_conflict", code)

	resp, _ = c.do(http.MethodPatch, location, `{"stock":100}`)
	require.Equal(t, http.StatusPreconditionRequired, resp.StatusCode)

	resp, body = c.do(http.MethodPost, "/api/v1/products", product)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	code, _ = errorCodes(t, body)
	require.Equal(t, "sku_taken", code)

	resp, body = c.do(http.MethodGet, "/", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "Running Shoes")
	require.NotEmpty(t, resp.Header.Get("Content-Security-Policy"))
	resp, _ = c.do(http.MethodGet, "/products/"+created["id"].(string), "")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, _ = c.do(http.MethodDelete, location, "", "If-Match", `"2"`)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, body = c.do(http.MethodGet, location, "")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	code, _ = errorCodes(t, body)
	require.Equal(t, "not_found", code)

	resp, _ = c.do(http.MethodPost, "/api/v1/products", product)
	require.Equal(t, http.StatusConflict, resp.StatusCode, "a deleted product keeps its SKU")
}

func TestValidationAndSearchOverHTTP(t *testing.T) {
	c := startApp(t)

	resp, body := c.do(http.MethodPost, "/api/v1/products", `{"sku":"bad sku","name":"","category":" ","price":"0","stock":-1,"weight_kg":"0.0001"}`)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	code, fields := errorCodes(t, body)
	require.Equal(t, "validation_failed", code)
	require.Equal(t, map[string]string{
		"sku": "invalid_format", "name": "required", "category": "required",
		"price": "not_positive", "stock": "out_of_range", "weight_kg": "too_many_decimals",
	}, fields)

	for _, p := range []string{
		`{"sku":"p-50","name":"Discount 50% off","category":"Deals","price":"1","stock":1,"weight_kg":"0"}`,
		`{"sku":"p-500","name":"Bundle 500 units","category":"Deals","price":"1","stock":1,"weight_kg":"0"}`,
	} {
		resp, body = c.do(http.MethodPost, "/api/v1/products", p)
		require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	}

	resp, body = c.do(http.MethodGet, "/api/v1/products?q=50%25", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, `"total":1`)
	require.Contains(t, body, `"sku":"P-50"`)

	resp, body = c.do(http.MethodGet, "/api/v1/categories", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.JSONEq(t, `["Deals"]`, body)

	resp, body = c.do(http.MethodPost, "/api/v1/products", `{"name":"`+strings.Repeat("x", 1<<20)+`"}`)
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	code, _ = errorCodes(t, body)
	require.Equal(t, "request_too_large", code)
}

func TestWeakIfMatchDoesNotModifyTheProduct(t *testing.T) {
	c := startApp(t)
	resp, _ := c.do(http.MethodPost, "/api/v1/products", `{"sku":"wk-001","name":"Kettle","category":"Home","price":"19.90","stock":4,"weight_kg":"1.200"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	location := resp.Header.Get("Location")
	require.Equal(t, `"1"`, resp.Header.Get("ETag"))

	stored := func() (version, stock int, status string) {
		require.NoError(t, c.pool.QueryRow(t.Context(),
			`SELECT version, stock, status FROM products WHERE id = $1::uuid`,
			location[strings.LastIndex(location, "/")+1:]).Scan(&version, &stock, &status))
		return version, stock, status
	}

	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		resp, body := c.do(method, location, `{"stock":99}`, "If-Match", `W/"1"`)

		require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
		code, _ := errorCodes(t, body)
		require.Equal(t, "precondition_failed", code)
		require.Contains(t, body, "weak entity tags are not accepted")
		version, stock, status := stored()
		require.Equal(t, 1, version, method)
		require.Equal(t, 4, stock, method)
		require.Equal(t, "ACTIVE", status, method)
	}
}
