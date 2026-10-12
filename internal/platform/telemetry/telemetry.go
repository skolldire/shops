package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

type Metrics interface {
	Counter(ctx context.Context, name string, value int64, attrs ...attribute.KeyValue)
	Gauge(ctx context.Context, name string, value float64, attrs ...attribute.KeyValue)
	Histogram(ctx context.Context, name string, value float64, attrs ...attribute.KeyValue)
}

type Tracer interface {
	Span(ctx context.Context, name string, fn func(ctx context.Context) error, attrs ...attribute.KeyValue) error
}

type Telemetry interface {
	Metrics
	Tracer
	Shutdown(ctx context.Context) error
}

func Noop() Telemetry {
	return noop{}
}

type noop struct{}

func (noop) Counter(context.Context, string, int64, ...attribute.KeyValue)     {}
func (noop) Gauge(context.Context, string, float64, ...attribute.KeyValue)     {}
func (noop) Histogram(context.Context, string, float64, ...attribute.KeyValue) {}
func (noop) Shutdown(context.Context) error                                    { return nil }

func (noop) Span(ctx context.Context, _ string, fn func(ctx context.Context) error, _ ...attribute.KeyValue) error {
	return fn(ctx)
}
