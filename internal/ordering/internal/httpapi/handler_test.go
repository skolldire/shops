package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/ordering/internal/httpapi"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/validation"
)

const (
	key       = "6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"
	productID = "11111111-1111-4111-8111-111111111111"
	body      = `{"items":[{"product_id":"` + productID + `","quantity":2,"expected_unit_price":"29.99"}],"payment_token":"pm_approved"}`
)

type fakeCheckout struct {
	user   string
	in     domain.CheckoutInput
	result app.CheckoutResult
	err    error
	calls  int
}

func (f *fakeCheckout) Checkout(_ context.Context, userID string, in domain.CheckoutInput) (app.CheckoutResult, error) {
	f.calls++
	f.user, f.in = userID, in
	if _, err := domain.NewCheckoutRequest(userID, in); err != nil {
		return app.CheckoutResult{}, err
	}
	return f.result, f.err
}

type fakeQueries struct {
	user           string
	id             string
	page, pageSize *int
	listed         int
	purchase       domain.Purchase
	err            error
}

func (f *fakeQueries) Purchase(_ context.Context, userID, id string) (domain.Purchase, error) {
	f.user, f.id = userID, id
	return f.purchase, f.err
}

func (f *fakeQueries) Purchases(_ context.Context, userID string, page, pageSize *int) (app.PurchasePage, error) {
	f.listed++
	f.user, f.page, f.pageSize = userID, page, pageSize
	if f.err != nil {
		return app.PurchasePage{}, f.err
	}
	p, err := domain.NewPage(page, pageSize)
	if err != nil {
		return app.PurchasePage{}, err
	}
	return app.PurchasePage{Items: []domain.Purchase{f.purchase}, Page: p.Page, PageSize: p.PageSize, Total: 1}, nil
}

func purchase(status domain.Status) domain.Purchase {
	p := domain.Purchase{
		ID: "99999999-9999-4999-8999-999999999999", UserID: "alice", Status: status, Currency: "USD",
		Total: decimal.RequireFromString("59.98"), PaymentToken: "pm_approved",
		CreatedAt: time.Date(2026, 10, 14, 12, 0, 0, 123456000, time.FixedZone("x", -5*3600)),
		Items: []domain.PurchaseItem{{ProductID: productID, SKU: "A-1", Name: "Alpha", UnitPrice: decimal.RequireFromString("29.99"),
			Quantity: 2, LineTotal: decimal.RequireFromString("59.98")}},
	}
	switch status {
	case domain.StatusConfirmed:
		p.PaymentRef = "fake_" + string(p.ID)
	case domain.StatusFailed:
		p.FailureReason = "card_declined"
	}
	return p
}

type server struct {
	h        http.Handler
	logs     *bytes.Buffer
	checkout *fakeCheckout
	queries  *fakeQueries
}

func newServer(t *testing.T) server {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	c, q := &fakeCheckout{}, &fakeQueries{}
	h, err := httpapi.New(c, q, []string{"alice", "bob"}, log)
	require.NoError(t, err)
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(httpx.MaxBodyBytes(1 << 20))
		h.Routes(r)
	})
	return server{h: r, logs: &logs, checkout: c, queries: q}
}

func (s server) do(method, path, payload string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(payload))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

func (s server) checkoutWith(payload string) *httptest.ResponseRecorder {
	return s.do(http.MethodPost, "/api/v1/checkout", payload, "X-Demo-User-ID", "alice", "Idempotency-Key", key)
}

type problem struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Errors []validation.FieldError
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problem {
	t.Helper()
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	var raw struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
		Detail string `json:"detail"`
		Errors []struct {
			Field, Code, Message string
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	p := problem{Status: raw.Status, Code: raw.Code, Detail: raw.Detail}
	for _, e := range raw.Errors {
		p.Errors = append(p.Errors, validation.FieldError{Field: e.Field, Code: e.Code, Message: e.Message})
	}
	return p
}

func TestDemoUserIsRequired(t *testing.T) {
	s := newServer(t)
	for _, user := range []string{"", "mallory", "ALICE"} {
		for _, req := range [][2]string{{http.MethodPost, "/api/v1/checkout"}, {http.MethodGet, "/api/v1/purchases"}, {http.MethodGet, "/api/v1/purchases/x"}} {
			rec := s.do(req[0], req[1], body, "X-Demo-User-ID", user, "Idempotency-Key", key)
			require.Equal(t, http.StatusBadRequest, rec.Code, req)
			p := decodeProblem(t, rec)
			require.Equal(t, "invalid_demo_user", p.Code)
			require.NotContains(t, rec.Body.String(), "alice")
			require.NotContains(t, rec.Body.String(), "bob")
		}
	}
	require.Zero(t, s.checkout.calls)
}

func TestCheckoutStatusByResult(t *testing.T) {
	tests := map[string]struct {
		result   app.CheckoutResult
		status   int
		location bool
	}{
		"created and confirmed": {app.CheckoutResult{Purchase: purchase(domain.StatusConfirmed), Kind: app.Created}, http.StatusCreated, true},
		"created and failed":    {app.CheckoutResult{Purchase: purchase(domain.StatusFailed), Kind: app.Created}, http.StatusCreated, true},
		"created and pending":   {app.CheckoutResult{Purchase: purchase(domain.StatusPending), Kind: app.Created}, http.StatusAccepted, true},
		"replayed":              {app.CheckoutResult{Purchase: purchase(domain.StatusConfirmed), Kind: app.Replayed}, http.StatusOK, false},
		"replayed failure":      {app.CheckoutResult{Purchase: purchase(domain.StatusFailed), Kind: app.Replayed}, http.StatusOK, false},
		"in progress":           {app.CheckoutResult{Purchase: purchase(domain.StatusPending), Kind: app.InProgress}, http.StatusAccepted, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := newServer(t)
			s.checkout.result = tc.result
			rec := s.checkoutWith(body)
			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			if tc.location {
				require.Equal(t, "/api/v1/purchases/99999999-9999-4999-8999-999999999999", rec.Header().Get("Location"))
			} else {
				require.Empty(t, rec.Header().Get("Location"))
			}
			require.NotContains(t, rec.Body.String(), "pm_approved")
			require.Equal(t, "alice", s.checkout.user)
		})
	}
}

func TestPurchaseRepresentation(t *testing.T) {
	s := newServer(t)
	s.checkout.result = app.CheckoutResult{Purchase: purchase(domain.StatusConfirmed), Kind: app.Created}
	rec := s.checkoutWith(body)
	require.JSONEq(t, `{"id":"99999999-9999-4999-8999-999999999999","status":"CONFIRMED","currency":"USD","total":"59.98",
		"payment_ref":"fake_99999999-9999-4999-8999-999999999999","failure_reason":null,"created_at":"2026-10-14T17:00:00.123456Z",
		"items":[{"product_id":"`+productID+`","sku":"A-1","name":"Alpha","unit_price":"29.99","quantity":2,"line_total":"59.98"}]}`, rec.Body.String())

	s.checkout.result = app.CheckoutResult{Purchase: purchase(domain.StatusFailed), Kind: app.Created}
	var got map[string]any
	require.NoError(t, json.Unmarshal(s.checkoutWith(body).Body.Bytes(), &got))
	require.Nil(t, got["payment_ref"])
	require.Equal(t, "card_declined", got["failure_reason"])
}

func TestCheckoutDecodesTheBody(t *testing.T) {
	s := newServer(t)
	s.checkout.result = app.CheckoutResult{Purchase: purchase(domain.StatusConfirmed), Kind: app.Created}
	rec := s.checkoutWith(`{"items":[{"product_id":"` + strings.ToUpper(productID) + `","quantity":2,"expected_unit_price":29.9}],"payment_token":"pm_approved","total":"0.01"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "client totals and unknown fields are ignored")
	require.Equal(t, key, s.checkout.in.IdempotencyKey)
	require.Equal(t, strings.ToUpper(productID), s.checkout.in.Items[0].ProductID, "the use case normalizes the id")
	require.Equal(t, "29.9", s.checkout.in.Items[0].ExpectedUnitPrice.String())
	require.Equal(t, 2, s.checkout.in.Items[0].Quantity)
	require.Equal(t, "pm_approved", s.checkout.in.PaymentToken)
}

func TestCheckoutRequestErrors(t *testing.T) {
	tests := map[string]struct {
		key, payload string
		status       int
		code         string
		fields       map[string]string
	}{
		"missing key":  {"", body, http.StatusBadRequest, "invalid_idempotency_key", nil},
		"invalid key":  {"not-a-uuid", body, http.StatusBadRequest, "invalid_idempotency_key", nil},
		"braces key":   {"{" + key + "}", body, http.StatusBadRequest, "invalid_idempotency_key", nil},
		"invalid json": {key, `{"items":`, http.StatusBadRequest, "invalid_request", nil},
		"trailing":     {key, body + `{}`, http.StatusBadRequest, "invalid_request", nil},
		"items object": {key, `{"items":{}}`, http.StatusBadRequest, "invalid_request", nil},
		"formats": {key, `{"items":[{"product_id":5,"quantity":"2.5","expected_unit_price":"1e3"}],"payment_token":7}`,
			http.StatusUnprocessableEntity, "validation_failed", map[string]string{
				"items[0].product_id": "invalid_format", "items[0].quantity": "invalid_format",
				"items[0].expected_unit_price": "invalid_format", "payment_token": "invalid_format",
			}},
		"rules": {key, `{"items":[{"product_id":"` + productID + `","quantity":100,"expected_unit_price":"0"}],"payment_token":""}`,
			http.StatusUnprocessableEntity, "validation_failed", map[string]string{
				"items[0].quantity": "out_of_range", "items[0].expected_unit_price": "not_positive", "payment_token": "required",
			}},
		"no items": {key, `{"payment_token":"pm_approved"}`, http.StatusUnprocessableEntity, "validation_failed", map[string]string{"items": "required"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := newServer(t)
			headers := []string{"X-Demo-User-ID", "alice"}
			if tc.key != "" {
				headers = append(headers, "Idempotency-Key", tc.key)
			}
			rec := s.do(http.MethodPost, "/api/v1/checkout", tc.payload, headers...)
			require.Equal(t, tc.status, rec.Code)
			if name == "formats" {
				require.Zero(t, s.checkout.calls, "a body with format errors never reaches the checkout")
			}
			p := decodeProblem(t, rec)
			require.Equal(t, tc.code, p.Code)
			if tc.fields != nil {
				got := map[string]string{}
				for _, fe := range p.Errors {
					got[fe.Field] = fe.Code
				}
				require.Equal(t, tc.fields, got)
			}
		})
	}
}

func TestAnInvalidExpectedPriceIsRejectedBeforeTheCheckout(t *testing.T) {
	s := newServer(t)
	s.checkout.result = app.CheckoutResult{Purchase: purchase(domain.StatusConfirmed), Kind: app.Created}
	for _, price := range []string{`"abc"`, `"1e3"`, `"29.99.1"`, `true`, `null`} {
		rec := s.checkoutWith(`{"items":[{"product_id":"` + productID + `","quantity":2,"expected_unit_price":` + price + `}],"payment_token":"pm_approved"}`)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, price)
		p := decodeProblem(t, rec)
		require.Equal(t, "validation_failed", p.Code)
		require.Len(t, p.Errors, 1, price)
		require.Equal(t, "items[0].expected_unit_price", p.Errors[0].Field)
		require.Equal(t, "invalid_format", p.Errors[0].Code)
	}
	require.Zero(t, s.checkout.calls, "no checkout operation runs for a malformed body")
}

func TestCheckoutTooLarge(t *testing.T) {
	s := newServer(t)
	rec := s.checkoutWith(`{"payment_token":"` + strings.Repeat("x", 1<<20) + `"}`)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Equal(t, "request_too_large", decodeProblem(t, rec).Code)
}

func TestCheckoutRejections(t *testing.T) {
	item := func(field, code, msg string) validation.Errors {
		return validation.Errors{{Field: field, Code: code, Message: msg}}
	}
	tests := map[string]struct {
		err    error
		status int
		code   string
		field  string
	}{
		"unavailable": {&app.RejectedError{Reason: app.ErrProductUnavailable, Errors: item("items[0].product_id", "product_unavailable", "gone")}, http.StatusUnprocessableEntity, "product_unavailable", "items[0].product_id"},
		"stock":       {&app.RejectedError{Reason: app.ErrInsufficientStock, Errors: item("items[0].quantity", "insufficient_stock", "only 1 available")}, http.StatusConflict, "insufficient_stock", "items[0].quantity"},
		"price":       {&app.RejectedError{Reason: app.ErrPriceChanged, Errors: item("items[0].expected_unit_price", "price_changed", "the current price is 31.50")}, http.StatusConflict, "price_changed", "items[0].expected_unit_price"},
		"conflict":    {app.ErrIdempotencyConflict, http.StatusConflict, "idempotency_conflict", ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := newServer(t)
			s.checkout.err = tc.err
			rec := s.checkoutWith(body)
			require.Equal(t, tc.status, rec.Code)
			p := decodeProblem(t, rec)
			require.Equal(t, tc.code, p.Code)
			if tc.field != "" {
				require.Equal(t, tc.field, p.Errors[0].Field)
			}
			require.Empty(t, s.logs.String())
		})
	}
}

func TestCheckoutTryAgainAndInternalError(t *testing.T) {
	s := newServer(t)
	s.checkout.err = errors.Join(app.ErrBusy, errors.New("ERROR: deadlock detected (SQLSTATE 40P01)"))
	rec := s.checkoutWith(body)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "1", rec.Header().Get("Retry-After"))
	require.Equal(t, "try_again", decodeProblem(t, rec).Code)
	require.NotContains(t, rec.Body.String(), "SQLSTATE")

	s = newServer(t)
	s.checkout.err = errors.New("ordering: insert purchase: relation purchases password=hunter2")
	rec = s.checkoutWith(body)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "internal_error", decodeProblem(t, rec).Code)
	require.NotContains(t, rec.Body.String(), "relation")
	require.Equal(t, 1, strings.Count(strings.TrimSpace(s.logs.String()), "\n")+1, "logged once")
}

func TestPurchaseQueries(t *testing.T) {
	s := newServer(t)
	s.queries.purchase = purchase(domain.StatusConfirmed)
	rec := s.do(http.MethodGet, "/api/v1/purchases/abc", "", "X-Demo-User-ID", "bob")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "bob", s.queries.user)
	require.Equal(t, "abc", s.queries.id)
	require.NotContains(t, rec.Body.String(), "pm_approved")

	s.queries.err = app.ErrPurchaseNotFound
	rec = s.do(http.MethodGet, "/api/v1/purchases/abc", "", "X-Demo-User-ID", "bob")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "not_found", decodeProblem(t, rec).Code)

	s = newServer(t)
	s.queries.purchase = purchase(domain.StatusFailed)
	rec = s.do(http.MethodGet, "/api/v1/purchases?page=2&page_size=5", "", "X-Demo-User-ID", "alice")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, [2]int{2, 5}, [2]int{*s.queries.page, *s.queries.pageSize})
	var page map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page["items"], 1)
	require.InDelta(t, 1, page["total"], 0)
	require.NotContains(t, rec.Body.String(), "pm_approved")

	rec = s.do(http.MethodGet, "/api/v1/purchases?page=x&page_size=1.5", "", "X-Demo-User-ID", "alice")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Len(t, decodeProblem(t, rec).Errors, 2, "every format error is reported")
}

func TestPurchasePagingInTheAPI(t *testing.T) {
	for query, want := range map[string]struct {
		status int
		fields map[string]string
		calls  int
		page   string
	}{
		"":                             {http.StatusOK, nil, 1, "1/20"},
		"?page=&page_size=":            {http.StatusOK, nil, 1, "1/20"},
		"?page=%202%20":                {http.StatusOK, nil, 1, "2/20"},
		"?page=0":                      {http.StatusUnprocessableEntity, map[string]string{"page": "out_of_range"}, 1, ""},
		"?page_size=0":                 {http.StatusUnprocessableEntity, map[string]string{"page_size": "out_of_range"}, 1, ""},
		"?page=-1&page_size=101":       {http.StatusUnprocessableEntity, map[string]string{"page": "out_of_range", "page_size": "out_of_range"}, 1, ""},
		"?page=x":                      {http.StatusUnprocessableEntity, map[string]string{"page": "invalid_format"}, 0, ""},
		"?page=1.5":                    {http.StatusUnprocessableEntity, map[string]string{"page": "invalid_format"}, 0, ""},
		"?page=%2B2":                   {http.StatusUnprocessableEntity, map[string]string{"page": "invalid_format"}, 0, ""},
		"?page=99999999999999999999":   {http.StatusUnprocessableEntity, map[string]string{"page": "out_of_range"}, 0, ""},
		"?page_size=99999999999999999": {http.StatusUnprocessableEntity, map[string]string{"page_size": "out_of_range"}, 0, ""},
	} {
		t.Run(query, func(t *testing.T) {
			s := newServer(t)
			s.queries.purchase = purchase(domain.StatusConfirmed)
			rec := s.do(http.MethodGet, "/api/v1/purchases"+query, "", "X-Demo-User-ID", "alice")
			require.Equal(t, want.status, rec.Code, rec.Body.String())
			require.Equal(t, want.calls, s.queries.listed, "a malformed parameter never reaches the purchase query")
			if want.fields != nil {
				p := decodeProblem(t, rec)
				require.Equal(t, "validation_failed", p.Code)
				got := map[string]string{}
				for _, fe := range p.Errors {
					got[fe.Field] = fe.Code
				}
				require.Equal(t, want.fields, got)
				return
			}
			var body struct {
				Page     int `json:"page"`
				PageSize int `json:"page_size"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, want.page, fmt.Sprintf("%d/%d", body.Page, body.PageSize))
		})
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	log, err := logger.New(logger.Config{Level: "error"}, &bytes.Buffer{})
	require.NoError(t, err)
	_, err = httpapi.New(nil, &fakeQueries{}, []string{"a"}, log)
	require.Error(t, err)
	_, err = httpapi.New(&fakeCheckout{}, &fakeQueries{}, nil, log)
	require.Error(t, err)
	_, err = httpapi.New(&fakeCheckout{}, &fakeQueries{}, []string{"a"}, nil)
	require.Error(t, err)
}
