package telemetry

import (
	"context"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func (p *Provider) HTTPMiddleware() func(http.Handler) http.Handler {
	return otelhttp.NewMiddleware("http.server",
		otelhttp.WithTracerProvider(p.tracerProvider),
		otelhttp.WithMeterProvider(p.meterProvider),
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		)),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method
		}),
	)
}

func TraceFields(ctx context.Context) map[string]any {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return map[string]any{}
	}
	return map[string]any{
		"trace_id": sc.TraceID().String(),
		"span_id":  sc.SpanID().String(),
	}
}
