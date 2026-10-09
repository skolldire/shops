package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/platform/telemetry"
)

type recorder struct {
	telemetry.Telemetry
	spans    []string
	counters map[string]int64
	hists    map[string]float64
	attrs    []attribute.KeyValue
}

func newRecorder() *recorder {
	return &recorder{Telemetry: telemetry.Noop(), counters: map[string]int64{}, hists: map[string]float64{}}
}

func (r *recorder) Counter(_ context.Context, name string, v int64, attrs ...attribute.KeyValue) {
	r.counters[name] += v
	r.attrs = attrs
}

func (r *recorder) Histogram(_ context.Context, name string, v float64, _ ...attribute.KeyValue) {
	r.hists[name] = v
}

func (r *recorder) Span(ctx context.Context, name string, fn func(context.Context) error, _ ...attribute.KeyValue) error {
	r.spans = append(r.spans, name)
	return fn(ctx)
}

func TestNoop(t *testing.T) {
	tel := telemetry.Noop()
	boom := errors.New("boom")

	tel.Counter(t.Context(), "c", 1)
	tel.Gauge(t.Context(), "g", 1)
	tel.Histogram(t.Context(), "h", 1)
	require.ErrorIs(t, tel.Span(t.Context(), "s", func(context.Context) error { return boom }), boom)
	require.NoError(t, tel.Shutdown(t.Context()))
}

func TestOperation(t *testing.T) {
	rec := newRecorder()
	op := telemetry.NewOperation(rec, "checkout")
	attr := attribute.String("outcome", "test")

	require.NoError(t, op.Execute(t.Context(), func(context.Context) error { return nil }, attr))
	boom := errors.New("declined")
	require.ErrorIs(t, op.Execute(t.Context(), func(context.Context) error { return boom }, attr), boom)

	require.Equal(t, []string{"checkout", "checkout"}, rec.spans)
	require.Equal(t, int64(2), rec.counters["checkout_total"])
	require.Equal(t, int64(1), rec.counters["checkout_errors_total"])
	require.Contains(t, rec.hists, "checkout_duration_seconds")
	require.Equal(t, []attribute.KeyValue{attr}, rec.attrs)
}
