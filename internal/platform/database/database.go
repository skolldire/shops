package database

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skolldire/shops/internal/platform/secret"
)

type Config struct {
	Host           string        `mapstructure:"host"`
	Port           int           `mapstructure:"port"`
	Name           string        `mapstructure:"name"`
	User           string        `mapstructure:"user"`
	Password       secret.Secret `mapstructure:"password"`
	SSLMode        string        `mapstructure:"ssl_mode"`
	MaxConns       int32         `mapstructure:"max_conns"`
	ConnectTimeout time.Duration `mapstructure:"connect_timeout"`
}

func Open(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	pc, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return pool, nil
}

func poolConfig(cfg Config) (*pgxpool.Config, error) {
	switch {
	case cfg.Host == "":
		return nil, errors.New("database: host is required")
	case cfg.Port < 1 || cfg.Port > 65535:
		return nil, fmt.Errorf("database: port %d out of range", cfg.Port)
	case cfg.Name == "":
		return nil, errors.New("database: name is required")
	case cfg.User == "":
		return nil, errors.New("database: user is required")
	case cfg.MaxConns <= 0:
		return nil, errors.New("database: max_conns must be positive")
	case cfg.ConnectTimeout <= 0:
		return nil, errors.New("database: connect_timeout must be positive")
	case !ValidSSLMode(cfg.SSLMode):
		return nil, fmt.Errorf("database: invalid ssl_mode %q", cfg.SSLMode)
	}

	pc, err := pgxpool.ParseConfig("host=localhost sslmode=" + cfg.SSLMode)
	if err != nil {
		return nil, fmt.Errorf("database: invalid ssl_mode %q", cfg.SSLMode)
	}

	cc := pc.ConnConfig
	cc.Host = cfg.Host
	cc.Port = uint16(cfg.Port)
	cc.Database = cfg.Name
	cc.User = cfg.User
	cc.Password = cfg.Password.Reveal()
	cc.ConnectTimeout = cfg.ConnectTimeout
	cc.TLSConfig = withServerName(cc.TLSConfig, cfg.Host)
	for _, fb := range cc.Fallbacks {
		fb.Host = cfg.Host
		fb.Port = uint16(cfg.Port)
		fb.TLSConfig = withServerName(fb.TLSConfig, cfg.Host)
	}

	pc.MaxConns = cfg.MaxConns
	return pc, nil
}

func withServerName(c *tls.Config, host string) *tls.Config {
	if c == nil {
		return nil
	}
	c = c.Clone()
	c.ServerName = host
	return c
}

func ValidSSLMode(mode string) bool {
	switch mode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
		return true
	}
	return false
}
