package config

import (
	"context"
	"testing"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/secret"
)

type hookTarget struct {
	Host     string        `mapstructure:"host"`
	Port     int           `mapstructure:"port"`
	Seed     bool          `mapstructure:"seed"`
	Password secret.Secret `mapstructure:"password"`
	Timeout  time.Duration `mapstructure:"timeout"`
}

func decodeWithPlaceholders(input map[string]any) (hookTarget, error) {
	var out hookTarget
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           &out,
		WeaklyTypedInput: true,
		DecodeHook:       decodeHook(context.Background(), fakeResolvers()),
	})
	if err != nil {
		return out, err
	}
	return out, dec.Decode(input)
}

func TestPlaceholderHook(t *testing.T) {
	out, err := decodeWithPlaceholders(map[string]any{
		"host":     "${USER}",
		"port":     "${PORT:-5432}",
		"seed":     "${SEED:-true}",
		"password": "${file:/run/secrets/db}",
	})

	require.NoError(t, err)
	require.Equal(t, "alice", out.Host)
	require.Equal(t, 5432, out.Port)
	require.True(t, out.Seed)
	require.Equal(t, "s3cr3t", out.Password.Reveal())
}

func TestPlaceholderHookSecretRules(t *testing.T) {
	for _, value := range []any{"hunter2", "pre-${USER}", 1234} {
		_, err := decodeWithPlaceholders(map[string]any{"password": value})
		require.ErrorContains(t, err, "'password' secret values must come from a placeholder", value)
	}

	_, err := decodeWithPlaceholders(map[string]any{"password": "${MISSING}"})
	require.ErrorContains(t, err, "${MISSING} is not set")
}

func TestDurationHooks(t *testing.T) {
	for _, value := range []any{"15s", "${TIMEOUT:-15s}"} {
		out, err := decodeWithPlaceholders(map[string]any{"timeout": value})
		require.NoError(t, err, value)
		require.Equal(t, 15*time.Second, out.Timeout, value)
	}

	for _, value := range []any{10, 1.5, "${TIMEOUT:-10}", "soon"} {
		_, err := decodeWithPlaceholders(map[string]any{"timeout": value})
		require.ErrorContains(t, err, "'timeout'", value)
	}
}

func TestPlaceholderHookPointerSecret(t *testing.T) {
	var out struct {
		Password *secret.Secret `mapstructure:"password"`
	}
	decode := func(value any) error {
		dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
			Result:     &out,
			DecodeHook: decodeHook(context.Background(), fakeResolvers()),
		})
		require.NoError(t, err)
		return dec.Decode(map[string]any{"password": value})
	}

	require.NoError(t, decode("${file:/run/secrets/db}"))
	require.Equal(t, "s3cr3t", out.Password.Reveal())
	require.ErrorContains(t, decode("hunter2"), "secret values must come from a placeholder")
}
