package config_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/config"
	"github.com/skolldire/shops/internal/platform/secret"
)

const leaked = "TOP-SECRET-123"

type dbConfig struct {
	Database struct {
		Host     string        `mapstructure:"host"`
		Port     int           `mapstructure:"port"`
		Password secret.Secret `mapstructure:"password"`
	} `mapstructure:"database"`
}

func loadDB(t *testing.T, content string) (dbConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	env := map[string]string{"PORT": leaked}
	return config.Load[dbConfig](context.Background(), path, config.Resolvers{
		"env":  config.Env(func(k string) (string, bool) { v, ok := env[k]; return v, ok }),
		"file": config.File(func(string) ([]byte, error) { return []byte(leaked + "\n"), nil }),
	})
}

func TestLoadSecretFromFile(t *testing.T) {
	cfg, err := loadDB(t, "database:\n  host: db\n  port: 5432\n  password: ${file:/run/secrets/db}\n")

	require.NoError(t, err)
	require.Equal(t, leaked, cfg.Database.Password.Reveal())
}

func TestLoadRejectsSecretDefaults(t *testing.T) {
	_, err := loadDB(t, "database:\n  password: ${DB_PASSWORD:-hunter2}\n")

	require.ErrorContains(t, err, "'database.password' secret placeholders cannot have a default")
	require.NotContains(t, err.Error(), "hunter2")
}

func TestLoadErrors(t *testing.T) {
	tests := map[string]string{
		"database:\n  host: db\n  bogus: 1\n": "'database' has invalid keys: bogus",
		"extra: 1\n":                          "invalid keys: extra",
		"database: [unclosed\n":               "parse",
		"database:\n  host: ${HOST}\n":        "'database.host' ${HOST} is not set",
		"database:\n  password: hunter2\n":    "'database.password' secret values must come from a placeholder",
		"database:\n  port: ${PORT}\n":        "'database.port' cannot parse value as 'int'",
	}
	for content, want := range tests {
		_, err := loadDB(t, content)
		require.ErrorContains(t, err, want, content)
		require.NotContains(t, err.Error(), leaked, content)
	}

	_, err := config.Load[dbConfig](context.Background(), filepath.Join(t.TempDir(), "nope.yaml"), nil)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestLoadReportsAllErrorsTogether(t *testing.T) {
	_, err := loadDB(t, "database:\n  host: ${HOST}\n  port: ${PORT}\n  password: hunter2\n  bogus: 1\n")

	for _, want := range []string{"'database.host'", "'database.port'", "'database.password'", "bogus"} {
		require.ErrorContains(t, err, want)
	}
	require.NotContains(t, err.Error(), leaked)
	require.NotContains(t, err.Error(), "hunter2")
}
