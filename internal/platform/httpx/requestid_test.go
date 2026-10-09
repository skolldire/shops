package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/httpx"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestRequestID(t *testing.T) {
	var seen string
	h := httpx.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestIDFrom(r.Context())
	}))

	tests := map[string]bool{
		"3f2b8c1e-9d4a-4b6f-8e2a-1c0d5e7f9a3b": true,
		"abcDEF123":                            true,
		strings.Repeat("a", 64):                true,
		"":                                     false,
		strings.Repeat("a", 65):                false,
		"abc;rm -rf":                           false,
		"abc\ndef":                             false,
	}
	for header, preserved := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Request-ID", header)
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		got := rec.Header().Get("X-Request-ID")
		require.Equal(t, seen, got, header)
		if preserved {
			require.Equal(t, header, got)
		} else {
			require.Regexp(t, uuidV4, got, header)
		}
	}
}
