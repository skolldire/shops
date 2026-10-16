package main

import (
	"context"
	"io/fs"
	"path/filepath"
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
	require.Equal(t, "/etc/shop/seed.csv", cfg.Demo.SeedFile)
}

func TestShippedConfigOverrides(t *testing.T) {
	env := map[string]string{"DB_PORT": "6000", "SEED_DEMO": "false", "SEED_FILE": "/data/demo.csv", "LOG_LEVEL": "debug", "TRACING_ENABLED": "true", "OTLP_ENDPOINT": "collector:4317"}

	cfg, err := loadConfig(t, "../../config/config.yaml", env, map[string]string{"/run/secrets/db_password": testPassword})

	require.NoError(t, err)
	require.Equal(t, 6000, cfg.Database.Port)
	require.False(t, cfg.Demo.Seed)
	require.Equal(t, "/data/demo.csv", cfg.Demo.SeedFile)
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
	require.Equal(t, "testdata/products.csv", cfg.Demo.SeedFile)
	require.FileExists(t, filepath.Join("..", "..", cfg.Demo.SeedFile))
}

func TestSeedFileIsRequiredOnlyWhenSeeding(t *testing.T) {
	cfg := shippedConfig(t)
	cfg.Demo.SeedFile = ""
	require.ErrorContains(t, cfg.Validate(), "demo.seed_file is required when demo.seed is true")

	cfg.Demo.Seed = false
	require.NoError(t, cfg.Validate())
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

func TestShippedConfigCheckoutDefaults(t *testing.T) {
	cfg := shippedConfig(t)
	require.Equal(t, []string{"alice", "bob", "carol"}, cfg.Demo.Users)
	require.Equal(t, 3*time.Second, cfg.Checkout.ReserveTimeout)
	require.Equal(t, 6*time.Second, cfg.Checkout.ChargeTimeout)
	require.Equal(t, 3*time.Second, cfg.Checkout.FinalizeTimeout)
	require.Equal(t, 30*time.Minute, cfg.Checkout.MaxPendingAge)
	require.Equal(t, RecoveryConfig{
		Interval: 10 * time.Second, BatchSize: 20, MinAge: 30 * time.Second, Lease: 30 * time.Second, DBTimeout: 2 * time.Second,
		BaseBackoff: 5 * time.Second, MaxBackoff: 5 * time.Minute, StuckAfterAttempts: 10,
	}, cfg.Recovery)

	local, err := loadConfig(t, "../../config/config.local.yaml", map[string]string{"DB_PASSWORD": testPassword}, nil)
	require.NoError(t, err)
	require.Equal(t, cfg.Demo.Users, local.Demo.Users)
	require.Equal(t, cfg.Checkout, local.Checkout)
	require.Equal(t, cfg.Recovery, local.Recovery)
}

func TestValidateDemoUsers(t *testing.T) {
	tests := map[string]struct {
		users []string
		want  string
	}{
		"empty list":  {nil, "demo.users must list at least one user"},
		"empty value": {[]string{"alice", " "}, "demo.users[1] must not be empty"},
		"repeated":    {[]string{"alice", "bob", "alice"}, "demo.users[2] repeats demo.users[0]"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := shippedConfig(t)
			cfg.Demo.Users = tc.users
			err := cfg.Validate()
			require.ErrorContains(t, err, tc.want)
			require.NotContains(t, err.Error(), "alice", "errors do not echo configured users")
		})
	}
}

func TestValidateCheckoutBudgets(t *testing.T) {
	cfg := shippedConfig(t)
	cfg.Checkout.ChargeTimeout = 0
	require.ErrorContains(t, cfg.Validate(), "checkout.charge_timeout must be positive")

	cfg = shippedConfig(t)
	cfg.HTTP.ShutdownTimeout = 13 * time.Second
	require.ErrorContains(t, cfg.Validate(), "must not exceed http.shutdown_timeout")
	cfg.HTTP.ShutdownTimeout = 14 * time.Second
	require.NoError(t, cfg.Validate(), "3s + 6s + 3s + 2s fits exactly")

	cfg = shippedConfig(t)
	cfg.HTTP.WriteTimeout = 10 * time.Second
	require.ErrorContains(t, cfg.Validate(), "must not exceed http.write_timeout")
}

func TestValidateRecovery(t *testing.T) {
	for want, change := range map[string]func(c *AppConfig){
		"recovery.interval must be positive":                             func(c *AppConfig) { c.Recovery.Interval = 0 },
		"recovery.db_timeout must be positive":                           func(c *AppConfig) { c.Recovery.DBTimeout = 0 },
		"recovery.batch_size must be at least 1":                         func(c *AppConfig) { c.Recovery.BatchSize = 0 },
		"recovery.stuck_after_attempts must be at least 1":               func(c *AppConfig) { c.Recovery.StuckAfterAttempts = 0 },
		"recovery.base_backoff must not exceed":                          func(c *AppConfig) { c.Recovery.BaseBackoff = 10 * time.Minute },
		"recovery.min_age must be greater than":                          func(c *AppConfig) { c.Recovery.MinAge = 14 * time.Second },
		"recovery.lease must be greater than":                            func(c *AppConfig) { c.Recovery.Lease = 11 * time.Second },
		"checkout.max_pending_age must be greater than recovery.min_age": func(c *AppConfig) { c.Checkout.MaxPendingAge = 30 * time.Second },
		"checkout.max_pending_age must be greater than recovery.lease":   func(c *AppConfig) { c.Recovery.Lease = 40 * time.Minute },
	} {
		cfg := shippedConfig(t)
		change(&cfg)
		require.ErrorContains(t, cfg.Validate(), want)
	}
	cfg := shippedConfig(t)
	cfg.Recovery.Lease = 12 * time.Second
	err := cfg.Validate()
	require.ErrorContains(t, err, "recovery.lease must be greater than 2 × recovery.db_timeout + 2 × checkout.charge_timeout + checkout.finalize_timeout + 2s")
	require.ErrorContains(t, err, "a lookup and a void at checkout.charge_timeout each")

	cfg.Recovery.Lease = 21 * time.Second
	require.ErrorContains(t, cfg.Validate(), "recovery.lease must be greater than", "2s + 2s + 6s + 6s + 3s + 2s is not enough")
	cfg.Recovery.MinAge, cfg.Recovery.Lease = 15*time.Second, 22*time.Second
	require.NoError(t, cfg.Validate(), "just above 3s + 6s + 3s + 2s and 2s + 2s + 6s + 6s + 3s + 2s")
}

func TestValidatePublicOrigins(t *testing.T) {
	cfg := shippedConfig(t)
	cfg.HTTP.PublicOrigins = []string{"https://shop.example", "http://localhost:8081"}
	require.NoError(t, cfg.Validate())
	cfg.HTTP.PublicOrigins = []string{"https://shop.example", "https://shop.example/secret-path"}
	err := cfg.Validate()
	require.ErrorContains(t, err, "http.public_origins[1] must be")
	require.NotContains(t, err.Error(), "secret-path", "the configured value is never reported")
}
