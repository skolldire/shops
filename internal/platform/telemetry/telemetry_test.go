package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/telemetry"
)

func TestNoop(t *testing.T) {
	tel := telemetry.Noop()
	boom := errors.New("boom")

	tel.Counter(t.Context(), "c", 1)
	tel.Gauge(t.Context(), "g", 1)
	tel.Histogram(t.Context(), "h", 1)
	require.ErrorIs(t, tel.Span(t.Context(), "s", func(context.Context) error { return boom }), boom)
	require.NoError(t, tel.Shutdown(t.Context()))
}
