package logger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/logger"
)

func newLogger(t *testing.T, level string, opts ...logger.Option) (logger.Service, func() []map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	log, err := logger.New(logger.Config{Level: level}, &buf, opts...)
	require.NoError(t, err)
	return log, func() []map[string]any {
		var out []map[string]any
		for dec := json.NewDecoder(&buf); dec.More(); {
			var line map[string]any
			require.NoError(t, dec.Decode(&line))
			out = append(out, line)
		}
		return out
	}
}

func TestLevelsAndKeys(t *testing.T) {
	log, read := newLogger(t, "info")

	log.Debug(t.Context(), "hidden", nil)
	log.Info(t.Context(), "started", map[string]any{"port": 8080})
	log.Warn(t.Context(), "slow", nil)

	got := read()
	require.Len(t, got, 2)
	require.Equal(t, "started", got[0]["message"])
	require.Equal(t, "INFO", got[0]["severity"])
	require.Contains(t, got[0], "timestamp")
	require.EqualValues(t, 8080, got[0]["port"])
	require.Equal(t, "WARN", got[1]["severity"])
}

func TestError(t *testing.T) {
	log, read := newLogger(t, "error")

	log.Error(t.Context(), errors.New("boom"), map[string]any{"component": "db"})
	log.Error(t.Context(), nil, nil)

	got := read()
	require.Len(t, got, 2)
	require.Equal(t, "ERROR", got[0]["severity"])
	require.Equal(t, "boom", got[0]["message"])
	require.Equal(t, "db", got[0]["component"])
	require.Equal(t, "unknown error", got[1]["message"])
}

func TestWithFields(t *testing.T) {
	base, read := newLogger(t, "info")
	child := base.WithFields(map[string]any{"module": "catalog"})

	child.Info(t.Context(), "child", map[string]any{"sku": "A1"})
	base.Info(t.Context(), "base", nil)

	got := read()
	require.Equal(t, "catalog", got[0]["module"])
	require.Equal(t, "A1", got[0]["sku"])
	require.NotContains(t, got[1], "module")
}

type ctxKey struct{}

func TestContextExtractor(t *testing.T) {
	extract := func(ctx context.Context) map[string]any {
		id, _ := ctx.Value(ctxKey{}).(string)
		return map[string]any{"request_id": id}
	}
	base, read := newLogger(t, "info", logger.WithContextExtractor(extract))
	ctx := context.WithValue(t.Context(), ctxKey{}, "req-1")

	base.Info(ctx, "direct", nil)
	base.WithFields(map[string]any{"module": "web"}).Error(ctx, errors.New("x"), nil)

	got := read()
	require.Equal(t, "req-1", got[0]["request_id"])
	require.Equal(t, "req-1", got[1]["request_id"])
}

func TestSanitizer(t *testing.T) {
	log, read := newLogger(t, "info")

	log.Info(t.Context(), "login password=hunter2 for user", map[string]any{
		"Password":      "hunter2",
		"authorization": "Bearer abc.def",
		"detail":        `connect failed: token: "xyz" header Authorization: Bearer abc.def`,
		"card":          "4111 1111 1111 1111",
		"sku":           "SKU-1234",
	})
	log.Error(t.Context(), errors.New("dial: api_key=k-123 refused"), nil)

	got := read()
	require.Equal(t, "login password=[REDACTED] for user", got[0]["message"])
	require.Equal(t, "[REDACTED]", got[0]["Password"])
	require.Equal(t, "[REDACTED]", got[0]["authorization"])
	require.Equal(t, `connect failed: token: [REDACTED] header Authorization: Bearer [REDACTED]`, got[0]["detail"])
	require.Equal(t, "[REDACTED]", got[0]["card"])
	require.Equal(t, "SKU-1234", got[0]["sku"])
	require.Equal(t, "dial: api_key=[REDACTED] refused", got[1]["message"])
}

func TestNewRejectsInvalidLevel(t *testing.T) {
	_, err := logger.New(logger.Config{Level: "verbose"}, &bytes.Buffer{})
	require.EqualError(t, err, `logger: invalid level "verbose"`)
}
