package main

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/config"
	"github.com/skolldire/shops/internal/platform/secret"
)

const testPassword = "Zx9-very-secret-password"

func loadConfig(t *testing.T, path string, env, files map[string]string) (AppConfig, error) {
	t.Helper()
	return config.Load[AppConfig](context.Background(), path, config.Resolvers{
		"env": config.Env(func(k string) (string, bool) { v, ok := env[k]; return v, ok }),
		"file": config.File(func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, fs.ErrNotExist
		}),
	})
}

func shippedConfig(t *testing.T) AppConfig {
	t.Helper()
	cfg, err := loadConfig(t, "../../config/config.yaml", nil, map[string]string{"/run/secrets/db_password": testPassword + "\n"})
	require.NoError(t, err)
	return cfg
}

func TestShippedConfigDefaults(t *testing.T) {
	cfg := shippedConfig(t)

	require.Equal(t, ":8080", cfg.HTTP.Addr)
	require.Equal(t, ":9100", cfg.Metrics.Addr)
	require.Equal(t, 15*time.Second, cfg.HTTP.ShutdownTimeout)
	require.Equal(t, "db", cfg.Database.Host)
	require.Equal(t, 5432, cfg.Database.Port)
	require.Equal(t, testPassword, cfg.Database.Password.Reveal())
	require.Equal(t, "info", cfg.Log.Level)
	require.Equal(t, "shops", cfg.Telemetry.ServiceName)
	require.False(t, cfg.Telemetry.Tracing.Enabled)
	require.True(t, cfg.Demo.Seed)
}

func TestShippedConfigOverrides(t *testing.T) {
	env := map[string]string{"DB_PORT": "6000", "SEED_DEMO": "false", "LOG_LEVEL": "debug", "TRACING_ENABLED": "true", "OTLP_ENDPOINT": "collector:4317"}

	cfg, err := loadConfig(t, "../../config/config.yaml", env, map[string]string{"/run/secrets/db_password": testPassword})

	require.NoError(t, err)
	require.Equal(t, 6000, cfg.Database.Port)
	require.False(t, cfg.Demo.Seed)
	require.Equal(t, "debug", cfg.Log.Level)
	require.True(t, cfg.Telemetry.Tracing.Enabled)
	require.Equal(t, "collector:4317", cfg.Telemetry.Tracing.Endpoint)
}

func TestShippedConfigRequiresSecretFile(t *testing.T) {
	_, err := loadConfig(t, "../../config/config.yaml", nil, nil)

	require.ErrorContains(t, err, "'database.password' ${file:/run/secrets/db_password}: read file /run/secrets/db_password")
}

func TestLocalConfigLoads(t *testing.T) {
	cfg, err := loadConfig(t, "../../config/config.local.yaml", map[string]string{"DB_PASSWORD": testPassword}, nil)

	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", cfg.Database.Host)
	require.Equal(t, "127.0.0.1:8080", cfg.HTTP.Addr)
}

func TestValidate(t *testing.T) {
	require.NoError(t, shippedConfig(t).Validate())

	cfg := shippedConfig(t)
	cfg.HTTP.Addr = "8080"
	cfg.Metrics.Addr = "8080"
	cfg.HTTP.ReadTimeout = 0
	cfg.Database.Port = 70000
	cfg.Database.Password = secret.Secret{}
	cfg.Database.SSLMode = "maybe"
	cfg.Database.MaxConns = 0
	cfg.Log.Level = "verbose"
	cfg.Telemetry.Tracing.Enabled = true
	cfg.Telemetry.Tracing.SamplingRate = 2

	err := cfg.Validate()

	for _, want := range []string{
		"http.addr", "metrics.addr must be", "metrics.addr must differ", "http.read_timeout",
		"database.port", "database.password", "database.ssl_mode", "database.max_conns",
		"log.level", "telemetry.tracing.endpoint", "telemetry.tracing.sampling_rate",
	} {
		require.ErrorContains(t, err, want)
	}
}
