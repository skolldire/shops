package database

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/secret"
)

func validConfig() Config {
	return Config{
		Host:           "db.internal",
		Port:           6543,
		Name:           "shop",
		User:           "shop",
		Password:       secret.New("pw"),
		SSLMode:        "disable",
		MaxConns:       7,
		ConnectTimeout: 3 * time.Second,
	}
}

func TestPoolConfigAssignsFieldsAndIgnoresEnv(t *testing.T) {
	t.Setenv("PGHOST", "env-host")
	t.Setenv("PGPASSWORD", "env-password")

	pc, err := poolConfig(validConfig())

	require.NoError(t, err)
	cc := pc.ConnConfig
	require.Equal(t, "db.internal", cc.Host)
	require.EqualValues(t, 6543, cc.Port)
	require.Equal(t, "shop", cc.Database)
	require.Equal(t, "shop", cc.User)
	require.Equal(t, "pw", cc.Password)
	require.Equal(t, 3*time.Second, cc.ConnectTimeout)
	require.EqualValues(t, 7, pc.MaxConns)
	require.Nil(t, cc.TLSConfig)
	require.Empty(t, cc.Fallbacks)
}

func TestPoolConfigSSLModes(t *testing.T) {
	tests := map[string]struct{ tls, fallbacks bool }{
		"disable":     {false, false},
		"allow":       {false, true},
		"prefer":      {true, true},
		"require":     {true, false},
		"verify-ca":   {true, false},
		"verify-full": {true, false},
	}
	for mode, want := range tests {
		t.Run(mode, func(t *testing.T) {
			cfg := validConfig()
			cfg.SSLMode = mode

			pc, err := poolConfig(cfg)

			require.NoError(t, err)
			cc := pc.ConnConfig
			require.Equal(t, want.tls, cc.TLSConfig != nil)
			if want.tls {
				require.Equal(t, "db.internal", cc.TLSConfig.ServerName)
			}
			require.Equal(t, want.fallbacks, len(cc.Fallbacks) > 0)
			for _, fb := range cc.Fallbacks {
				require.Equal(t, "db.internal", fb.Host)
				require.EqualValues(t, 6543, fb.Port)
				if fb.TLSConfig != nil {
					require.Equal(t, "db.internal", fb.TLSConfig.ServerName)
				}
			}
		})
	}
}

func TestPoolConfigRejectsInvalid(t *testing.T) {
	tests := map[string]func(*Config){
		"empty host":        func(c *Config) { c.Host = "" },
		"port zero":         func(c *Config) { c.Port = 0 },
		"port too high":     func(c *Config) { c.Port = 70000 },
		"empty name":        func(c *Config) { c.Name = "" },
		"empty user":        func(c *Config) { c.User = "" },
		"zero max conns":    func(c *Config) { c.MaxConns = 0 },
		"zero timeout":      func(c *Config) { c.ConnectTimeout = 0 },
		"invalid sslmode":   func(c *Config) { c.SSLMode = "bogus" },
		"sslmode injection": func(c *Config) { c.SSLMode = "disable host=evil" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			mutate(&cfg)

			_, err := poolConfig(cfg)

			require.Error(t, err)
		})
	}
}
