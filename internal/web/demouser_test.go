package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
)

func newOrderingUI(t *testing.T, orders *fakeOrdering, cat *fakeCatalog) (http.Handler, *fakeOrdering) {
	t.Helper()
	if cat == nil {
		cat = &fakeCatalog{}
	}
	if cat.products == nil {
		cat.products = map[string]catalog.Product{}
	}
	log, _ := newTestLogger(t)
	ui, err := New(Deps{Catalog: cat, Ordering: orders, Users: testUsers, Log: log, MaxImportBodyBytes: testMaxImportBodyBytes})
	require.NoError(t, err)
	return ui.Routes(), orders
}

func as(user string) []string {
	return []string{"Cookie", userCookie + "=" + url.QueryEscape(user)}
}

func submit(h http.Handler, path string, form url.Values, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", testOrigin)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCurrentUserComesFromTheCookie(t *testing.T) {
	h, orders := newOrderingUI(t, &fakeOrdering{}, nil)
	for cookie, want := range map[string]string{"": "alice", "bob": "bob", "carol": "carol", "mallory": "alice", "%zz": "alice", "BOB": "alice"} {
		orders.calls = nil
		headers := []string{}
		if cookie != "" {
			headers = []string{"Cookie", userCookie + "=" + cookie}
		}
		rec := get(h, "/cart", headers...)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, want, orders.calls[0].user, cookie)
		require.Contains(t, rec.Body.String(), `<option value="`+want+`" selected>`)
	}
}

func TestSelectingADemoUserSetsTheCookie(t *testing.T) {
	h, _ := newOrderingUI(t, &fakeOrdering{}, nil)
	rec := submit(h, "/demo-user", url.Values{"user": {"carol"}, "next": {"/products/p1?x=1"}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/products/p1?x=1", rec.Header().Get("Location"))
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	c := cookies[0]
	require.Equal(t, userCookie, c.Name)
	require.Equal(t, "carol", c.Value)
	require.Equal(t, "/", c.Path)
	require.True(t, c.HttpOnly)
	require.Equal(t, http.SameSiteLaxMode, c.SameSite)

	for _, next := range []string{"//evil.example/x", "https://evil.example", `/\evil.example`, "relative", ""} {
		rec = submit(h, "/demo-user", url.Values{"user": {"bob"}, "next": {next}})
		require.Equal(t, "/", rec.Header().Get("Location"), next)
	}

	rec = submit(h, "/demo-user", url.Values{"user": {"mallory"}})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, rec.Result().Cookies())
	require.NotContains(t, rec.Body.String(), "mallory")
}

func TestHeaderShowsTheCartCount(t *testing.T) {
	orders := &fakeOrdering{}
	orders.cart.Lines = make([]ordering.CartLine, 3)
	h, _ := newOrderingUI(t, orders, nil)
	body := get(h, "/", as("bob")...).Body.String()
	require.Contains(t, body, ">Cart (3)</a>")
	require.Equal(t, []string{"bob"}, orders.counted)
	require.Contains(t, body, `<input type="hidden" name="next" value="/">`)
}

func TestHeaderWithoutCountWhenTheCartCannotBeRead(t *testing.T) {
	orders := &fakeOrdering{countErr: errBoom}
	cat := &fakeCatalog{}
	log, logs := newTestLogger(t)
	cat.products = map[string]catalog.Product{}
	ui, err := New(Deps{Catalog: cat, Ordering: orders, Users: testUsers, Log: log, MaxImportBodyBytes: testMaxImportBodyBytes})
	require.NoError(t, err)

	rec := get(ui.Routes(), "/")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), ">Cart</a>")
	require.NotContains(t, rec.Body.String(), "Cart (0)")
	require.Equal(t, 1, strings.Count(logs.String(), "count the cart for the header"))
	require.NotContains(t, rec.Body.String(), "hunter2")
}

func TestErrorPagesUseTheReducedHeader(t *testing.T) {
	orders := &fakeOrdering{countErr: errBoom}
	h, _ := newOrderingUI(t, orders, nil)
	rec := get(h, "/no-such-page")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), ">Cart</a>")
	require.Empty(t, orders.counted, "error pages never query the cart")

	orders.cartErr = errBoom
	rec = get(h, "/cart")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), "Something went wrong")
	require.Empty(t, orders.counted)
}
