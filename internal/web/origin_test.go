package web

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func sameOriginRequest(host string, secure bool, headers ...string) bool {
	req := httptest.NewRequest(http.MethodPost, "/cart/items", nil)
	req.Host = host
	if secure {
		req.TLS = &tls.ConnectionState{}
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	return sameOrigin(req)
}

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		secure  bool
		headers []string
		want    bool
	}{
		{"same origin", "localhost:8080", false, []string{"Origin", "http://localhost:8080"}, true},
		{"case and default port", "Example.com", false, []string{"Origin", "HTTP://EXAMPLE.COM:80"}, true},
		{"https default port", "example.com:443", true, []string{"Origin", "https://example.com"}, true},
		{"ipv6", "[::1]:8080", false, []string{"Origin", "http://[::1]:8080"}, true},
		{"foreign host", "localhost:8080", false, []string{"Origin", "http://evil.example"}, false},
		{"prefix is not enough", "localhost:8080", false, []string{"Origin", "http://localhost:8080.evil.example"}, false},
		{"other port", "localhost:8080", false, []string{"Origin", "http://localhost:9090"}, false},
		{"other scheme", "localhost:8080", false, []string{"Origin", "https://localhost:8080"}, false},
		{"null origin", "localhost:8080", false, []string{"Origin", "null"}, false},
		{"unparsable origin", "localhost:8080", false, []string{"Origin", "http://%zz"}, false},
		{"origin with a path", "localhost:8080", false, []string{"Origin", "http://localhost:8080/cart"}, false},
		{"origin with credentials", "localhost:8080", false, []string{"Origin", "http://user@localhost:8080"}, false},
		{"empty origin", "localhost:8080", false, []string{"Origin", ""}, false},
		{"two origins", "localhost:8080", false, []string{"Origin", "http://localhost:8080", "Origin", "http://localhost:8080"}, false},
		{"origin wins over referer", "localhost:8080", false, []string{"Origin", "http://evil.example", "Referer", "http://localhost:8080/cart"}, false},
		{"only referer", "localhost:8080", false, []string{"Referer", "http://localhost:8080/products/1?x=y"}, true},
		{"foreign referer", "localhost:8080", false, []string{"Referer", "http://evil.example/localhost:8080"}, false},
		{"relative referer", "localhost:8080", false, []string{"Referer", "/cart"}, false},
		{"no headers", "localhost:8080", false, nil, false},
		{"forwarded headers are ignored", "localhost:8080", false, []string{"Origin", "https://shop.example", "X-Forwarded-Host", "shop.example", "X-Forwarded-Proto", "https"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sameOriginRequest(tc.host, tc.secure, tc.headers...))
		})
	}
}

func TestCrossOriginFormsAreRejected(t *testing.T) {
	cat := &fakeCatalog{}
	h, orders := newOrderingUI(t, &fakeOrdering{}, cat)

	send := func(path string, form url.Values, headers ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/admin/products", "/cart/items", "/checkout", "/demo-user", "/admin/products/p1/delete", adminImports} {
		for _, headers := range [][]string{{"Origin", "http://evil.example"}, {"Referer", "http://evil.example/form"}, {"Origin", "null"}, nil} {
			rec := send(path, validForm(), headers...)
			require.Equal(t, http.StatusForbidden, rec.Code, path)
			require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
			require.Contains(t, rec.Body.String(), "Request blocked")
			require.Empty(t, rec.Header().Values("Set-Cookie"))
		}
	}
	require.Empty(t, cat.created.SKU)
	require.Empty(t, orders.calls)

	require.Equal(t, http.StatusSeeOther, send("/admin/products", validForm(), "Referer", testOrigin+"/admin/products/new").Code)
	require.Equal(t, "RS-001", strings.ToUpper(cat.created.SKU))
	require.Equal(t, http.StatusOK, get(h, "/cart").Code, "safe methods are not checked")
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusForbidden, rec.Code)
}

func TestPublicOriginsAreAcceptedBehindAProxy(t *testing.T) {
	public, ok := parseOrigin("https://shop.example")
	require.True(t, ok)
	post := func(origin string) bool {
		req := httptest.NewRequest(http.MethodPost, "/cart/items", nil)
		req.Host = "app:8080"
		req.Header.Set("Origin", origin)
		return sameOrigin(req, public)
	}
	require.True(t, post("https://shop.example"), "the proxy rewrote the host, the public origin is configured")
	require.True(t, post("https://shop.example:443"))
	require.True(t, post("http://app:8080"), "the request's own origin is still accepted")
	require.False(t, post("http://shop.example"), "another scheme is another origin")
	require.False(t, post("https://evil.example"))
}

func TestPublicOriginsMustBeBareOrigins(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://shop.example": true, "http://localhost:8081": true,
		"https://shop.example/": false, "https://shop.example/path": false, "ftp://shop.example": false,
		"shop.example": false, "https://user@shop.example": false, "": false,
	} {
		require.Equal(t, want, ValidPublicOrigin(raw), raw)
	}
	cat := &fakeCatalog{}
	log, _ := newTestLogger(t)
	_, err := New(Deps{Catalog: cat, Ordering: &fakeOrdering{}, Users: testUsers, Log: log, MaxImportBodyBytes: 1, PublicOrigins: []string{"shop.example"}})
	require.Error(t, err)
}
