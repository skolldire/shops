package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/catalog/internal/httpapi"
	"github.com/skolldire/shops/internal/platform/logger"
)

type fakeInventory struct {
	paging domain.Paging
	err    error
}

func (f *fakeInventory) Movements(_ context.Context, productID string, p domain.Paging) (app.MovementPage, error) {
	f.paging = p
	if f.err != nil {
		return app.MovementPage{}, f.err
	}
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	return app.MovementPage{Items: []domain.Movement{
		{ID: 2, ProductID: productID, Delta: -1, StockAfter: 4, ReservedDelta: -1, ReservedAfter: 2, Reason: "sale", Reference: "p-1", DBUser: "shop", TxID: "900", CreatedAt: at},
		{ID: 1, ProductID: productID, Delta: 5, StockAfter: 5, Reason: "initial_stock", DBUser: "shop", TxID: "800", CreatedAt: at},
	}, Page: p.Page, PageSize: p.PageSize, Total: 2}, nil
}

func movementsServer(t *testing.T, inv *fakeInventory) http.Handler {
	t.Helper()
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	h, err := httpapi.NewMovements(inv, log)
	require.NoError(t, err)
	r := chi.NewRouter()
	r.Route("/api/v1", h.Routes)
	return r
}

func TestMovementsNewestFirstWithPaging(t *testing.T) {
	inv := &fakeInventory{}
	rec := do(movementsServer(t, inv), http.MethodGet, "/api/v1/products/"+id+"/movements?page=2&page_size=5", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, domain.Paging{Page: 2, PageSize: 5}, inv.paging)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.InDelta(t, 2, body["page"], 0)
	require.InDelta(t, 5, body["page_size"], 0)
	require.InDelta(t, 2, body["total"], 0)
	items := body["items"].([]any)
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	require.Equal(t, "sale", first["reason"])
	require.Equal(t, "p-1", first["reference"])
	require.InDelta(t, -1, first["delta"], 0)
	require.InDelta(t, -1, first["reserved_delta"], 0)
	require.InDelta(t, 2, first["reserved_after"], 0)
	require.InDelta(t, 0, second["reserved_delta"], 0)
	require.Nil(t, second["reference"])
	require.Equal(t, "shop", second["db_user"])
}

func TestMovementsDefaultsAndErrors(t *testing.T) {
	inv := &fakeInventory{}
	h := movementsServer(t, inv)
	require.Equal(t, http.StatusOK, do(h, http.MethodGet, "/api/v1/products/"+id+"/movements", "").Code)
	require.Equal(t, domain.Paging{Page: 1, PageSize: 20}, inv.paging)

	rec := do(h, http.MethodGet, "/api/v1/products/"+id+"/movements?page_size=101&page=x", "")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	p := decodeProblem(t, rec)
	require.Equal(t, "validation_failed", p.Code)
	require.Len(t, p.Errors, 2)

	inv.err = app.ErrNotFound
	rec = do(h, http.MethodGet, "/api/v1/products/"+id+"/movements", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "not_found", decodeProblem(t, rec).Code)
}
