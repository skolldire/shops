package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/platform/logger"
)

func Recover(log logger.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				log.Error(r.Context(), fmt.Errorf("panic recovered: %v", v), map[string]any{
					"method": r.Method,
					"path":   r.URL.Path,
					"stack":  string(debug.Stack()),
				})
				WriteError(w, r, ErrorResponse{
					Status: http.StatusInternalServerError,
					Code:   "internal_error",
					Detail: "An unexpected error occurred.",
				})
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func Logging(log logger.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			log.Info(r.Context(), "http request", map[string]any{
				"method":      r.Method,
				"path":        r.URL.Path,
				"route":       routePattern(r),
				"status":      sw.statusCode(),
				"bytes":       sw.bytes,
				"duration_ms": float64(time.Since(start).Microseconds()) / 1000,
			})
		})
	}
}

func MaxBodyBytes(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		return rctx.RoutePattern()
	}
	return ""
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusWriter) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusWriter) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

func (s *statusWriter) statusCode() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}
