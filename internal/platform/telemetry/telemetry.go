package telemetry

import (
	"context"
	"time"

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

type Operation struct {
	tel  Telemetry
	name string
}

func NewOperation(tel Telemetry, name string) *Operation {
	return &Operation{tel: tel, name: name}
}

func (o *Operation) Execute(ctx context.Context, fn func(ctx context.Context) error, attrs ...attribute.KeyValue) error {
	start := time.Now()
	return o.tel.Span(ctx, o.name, func(ctx context.Context) error {
		err := fn(ctx)
		o.tel.Counter(ctx, o.name+"_total", 1, attrs...)
		o.tel.Histogram(ctx, o.name+"_duration_seconds", time.Since(start).Seconds(), attrs...)
		if err != nil {
			o.tel.Counter(ctx, o.name+"_errors_total", 1, attrs...)
		}
		return err
	}, attrs...)
}
