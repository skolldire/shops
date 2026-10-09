package httpx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/httpx"
)

type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	Instance  string `json:"instance"`
	RequestID string `json:"request_id"`
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problem {
	t.Helper()
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	var p problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	return p
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()

	httpx.WriteJSON(rec, http.StatusCreated, map[string]int{"n": 1})

	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.JSONEq(t, `{"n":1}`, rec.Body.String())
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/products/42", nil)

	httpx.WriteError(rec, req, httpx.ErrorResponse{Status: http.StatusConflict, Code: "conflict", Detail: "SKU already exists."})

	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, problem{
		Type: "about:blank", Title: "Conflict", Status: 409, Detail: "SKU already exists.",
		Code: "conflict", Instance: "/api/v1/products/42",
	}, decodeProblem(t, rec))
	require.NotContains(t, rec.Body.String(), "request_id")
}

func TestWriteErrorFallsBackTo500(t *testing.T) {
	rec := httptest.NewRecorder()

	httpx.WriteError(rec, httptest.NewRequest(http.MethodGet, "/", nil), httpx.ErrorResponse{Status: 200})

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "Internal Server Error", decodeProblem(t, rec).Title)
}
