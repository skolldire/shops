package httpx_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
)

func newLogger(t *testing.T) (logger.Service, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	log, err := logger.New(logger.Config{Level: "debug"}, &buf)
	require.NoError(t, err)
	return log, &buf
}

func TestRecover(t *testing.T) {
	log, logs := newLogger(t)
	h := httpx.RequestID(httpx.Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom: internal detail")
	})))
	req := httptest.NewRequest(http.MethodGet, "/explode", nil)
	req.Header.Set("X-Request-ID", "req123")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	p := decodeProblem(t, rec)
	require.Equal(t, "internal_error", p.Code)
	require.Equal(t, "req123", p.RequestID)
	require.NotContains(t, rec.Body.String(), "boom")
	require.Contains(t, logs.String(), "panic recovered: boom: internal detail")
	var line struct {
		Stack []string `json:"stack"`
	}
	require.NoError(t, json.Unmarshal(logs.Bytes(), &line))
	require.Greater(t, len(line.Stack), 1)
	require.Contains(t, line.Stack[0], "goroutine")
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	log, _ := newLogger(t)
	h := httpx.Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
}

func TestLogging(t *testing.T) {
	tests := map[string]struct {
		handler       http.HandlerFunc
		status, bytes int
	}{
		"explicit status": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, "short")
		}, http.StatusTeapot, 5},
		"implicit 200": {func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") }, 200, 5},
		"no body":      {func(http.ResponseWriter, *http.Request) {}, 200, 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			log, logs := newLogger(t)

			httpx.Logging(log)(tt.handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/things", nil))

			var line map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &line))
			require.Equal(t, "http request", line["message"])
			require.Equal(t, "POST", line["method"])
			require.Equal(t, "/things", line["path"])
			require.EqualValues(t, tt.status, line["status"])
			require.EqualValues(t, tt.bytes, line["bytes"])
			require.Contains(t, line, "duration_ms")
		})
	}
}

func TestMaxBodyBytes(t *testing.T) {
	h := httpx.MaxBodyBytes(4)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("too long")))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}
