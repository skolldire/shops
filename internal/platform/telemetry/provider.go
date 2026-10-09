package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/skolldire/shops/internal/platform/logger"
)

type Config struct {
	ServiceName    string        `mapstructure:"service_name"`
	ServiceVersion string        `mapstructure:"service_version"`
	Tracing        TracingConfig `mapstructure:"tracing"`
}

type TracingConfig struct {
	Enabled      bool    `mapstructure:"enabled"`
	Endpoint     string  `mapstructure:"endpoint"`
	Insecure     bool    `mapstructure:"insecure"`
	SamplingRate float64 `mapstructure:"sampling_rate"`
}

type Provider struct {
	log            logger.Service
	meterProvider  *sdkmetric.MeterProvider
	tracerProvider *sdktrace.TracerProvider
	registry       *prometheus.Registry
	meter          metric.Meter
	tracer         trace.Tracer
	counters       sync.Map
	gauges         sync.Map
	histograms     sync.Map
}

func New(ctx context.Context, cfg Config, log logger.Service) (*Provider, error) {
	var exporter sdktrace.SpanExporter
	if cfg.Tracing.Enabled {
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Tracing.Endpoint)}
		if cfg.Tracing.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exp, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: create trace exporter: %w", err)
		}
		exporter = exp
	}
	return newProvider(cfg, log, exporter)
}

func newProvider(cfg Config, log logger.Service, exporter sdktrace.SpanExporter) (*Provider, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: service_name is required")
	}
	if log == nil {
		return nil, errors.New("telemetry: logger is required")
	}
	res := resource.NewSchemaless(
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
	)

	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	reader, err := otelprom.New(otelprom.WithRegisterer(registry), otelprom.WithoutScopeInfo())
	if err != nil {
		return nil, fmt.Errorf("telemetry: create prometheus exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.*"},
			sdkmetric.Stream{AttributeFilter: attribute.NewDenyKeysFilter(
				semconv.ServerAddressKey,
				semconv.ServerPortKey,
			)},
		)),
	)

	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if exporter != nil {
		rate := cfg.Tracing.SamplingRate
		if rate <= 0 {
			rate = 1
		}
		tpOpts = append(tpOpts,
			sdktrace.WithBatcher(exporter),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(rate))),
		)
	} else {
		tpOpts = append(tpOpts, sdktrace.WithSampler(sdktrace.NeverSample()))
	}
	tp := sdktrace.NewTracerProvider(tpOpts...)

	return &Provider{
		log:            log,
		meterProvider:  mp,
		tracerProvider: tp,
		registry:       registry,
		meter:          mp.Meter(cfg.ServiceName),
		tracer:         tp.Tracer(cfg.ServiceName),
	}, nil
}

func (p *Provider) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{})
}

func (p *Provider) MeterProvider() metric.MeterProvider {
	return p.meterProvider
}

func (p *Provider) TracerProvider() trace.TracerProvider {
	return p.tracerProvider
}

func (p *Provider) Counter(ctx context.Context, name string, value int64, attrs ...attribute.KeyValue) {
	c, err := instrument(&p.counters, name, func() (metric.Int64Counter, error) { return p.meter.Int64Counter(name) })
	if err != nil {
		p.instrumentFailed(ctx, name, err)
		return
	}
	c.Add(ctx, value, metric.WithAttributes(attrs...))
}

func (p *Provider) Gauge(ctx context.Context, name string, value float64, attrs ...attribute.KeyValue) {
	g, err := instrument(&p.gauges, name, func() (metric.Float64Gauge, error) { return p.meter.Float64Gauge(name) })
	if err != nil {
		p.instrumentFailed(ctx, name, err)
		return
	}
	g.Record(ctx, value, metric.WithAttributes(attrs...))
}

func (p *Provider) Histogram(ctx context.Context, name string, value float64, attrs ...attribute.KeyValue) {
	h, err := instrument(&p.histograms, name, func() (metric.Float64Histogram, error) { return p.meter.Float64Histogram(name) })
	if err != nil {
		p.instrumentFailed(ctx, name, err)
		return
	}
	h.Record(ctx, value, metric.WithAttributes(attrs...))
}

func (p *Provider) Span(ctx context.Context, name string, fn func(ctx context.Context) error, attrs ...attribute.KeyValue) error {
	ctx, span := p.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	defer span.End()

	if err := fn(ctx); err != nil {
		msg := logger.Sanitize(err.Error())
		span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
			semconv.ExceptionTypeKey.String(fmt.Sprintf("%T", err)),
			semconv.ExceptionMessageKey.String(msg),
		))
		span.SetStatus(codes.Error, msg)
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p *Provider) Shutdown(ctx context.Context) error {
	return errors.Join(p.tracerProvider.Shutdown(ctx), p.meterProvider.Shutdown(ctx))
}

func (p *Provider) instrumentFailed(ctx context.Context, name string, err error) {
	p.log.Warn(ctx, "telemetry: instrument unavailable", map[string]any{"instrument": name, "error": err.Error()})
}

func instrument[T any](cache *sync.Map, name string, create func() (T, error)) (T, error) {
	if v, ok := cache.Load(name); ok {
		return v.(T), nil
	}
	inst, err := create()
	if err != nil {
		return inst, err
	}
	actual, _ := cache.LoadOrStore(name, inst)
	return actual.(T), nil
}
