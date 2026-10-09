package config_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/config"
)

type appConfig struct {
	Addr    string        `mapstructure:"addr"`
	Timeout time.Duration `mapstructure:"timeout"`
}

func (c appConfig) Validate() error {
	if c.Addr == ":0" {
		return errors.New("addr must not be :0")
	}
	return nil
}

func load(t *testing.T, content string) (appConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return config.Load[appConfig](context.Background(), path, config.Resolvers{
		"env": config.Env(func(string) (string, bool) { return "", false }),
	})
}

func TestLoad(t *testing.T) {
	cfg, err := load(t, "addr: ${HTTP_ADDR:-:8080}\ntimeout: 5s\n")

	require.NoError(t, err)
	require.Equal(t, appConfig{Addr: ":8080", Timeout: 5 * time.Second}, cfg)
}

func TestLoadCallsValidate(t *testing.T) {
	_, err := load(t, "addr: :0\n")

	require.ErrorContains(t, err, "invalid configuration: addr must not be :0")
}
