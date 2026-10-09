package httpx

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/skolldire/shops/internal/platform/logger"
)

var standardMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

func NewRouter(log logger.Service) *chi.Mux {
	r := chi.NewRouter()
	r.Use(RequestID, Logging(log), Recover(log), annotateRoute)
	r.NotFound(notFound)
	r.MethodNotAllowed(methodNotAllowed(r))
	return r
}

func notFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, ErrorResponse{
		Status: http.StatusNotFound,
		Code:   "not_found",
		Detail: "The requested resource was not found.",
	})
}

func methodNotAllowed(root *chi.Mux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var allowed []string
		for _, m := range standardMethods {
			if root.Find(chi.NewRouteContext(), m, r.URL.Path) != "" {
				allowed = append(allowed, m)
			}
		}
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		WriteError(w, r, ErrorResponse{
			Status: http.StatusMethodNotAllowed,
			Code:   "method_not_allowed",
			Detail: "The method is not allowed for the requested resource.",
		})
	}
}

func annotateRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		pattern := routePattern(r)
		if pattern == "" {
			return
		}
		route := semconv.HTTPRoute(pattern)
		span := trace.SpanFromContext(r.Context())
		span.SetName(r.Method + " " + pattern)
		span.SetAttributes(route)
		if labeler, ok := otelhttp.LabelerFromContext(r.Context()); ok {
			labeler.Add(route)
		}
	})
}
