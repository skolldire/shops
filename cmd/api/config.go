package main

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/health"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

type AppConfig struct {
	HTTP      HTTPConfig       `mapstructure:"http"`
	Metrics   MetricsConfig    `mapstructure:"metrics"`
	Health    health.Config    `mapstructure:"health"`
	Database  database.Config  `mapstructure:"database"`
	Log       logger.Config    `mapstructure:"log"`
	Telemetry telemetry.Config `mapstructure:"telemetry"`
	Import    ImportConfig     `mapstructure:"import"`
	Demo      DemoConfig       `mapstructure:"demo"`
}

type HTTPConfig struct {
	Addr              string        `mapstructure:"addr"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
	ReadTimeout       time.Duration `mapstructure:"read_timeout"`
	WriteTimeout      time.Duration `mapstructure:"write_timeout"`
	IdleTimeout       time.Duration `mapstructure:"idle_timeout"`
	ShutdownTimeout   time.Duration `mapstructure:"shutdown_timeout"`
}

type MetricsConfig struct {
	Addr string `mapstructure:"addr"`
}

type ImportConfig struct {
	MaxBytes int64 `mapstructure:"max_bytes"`
	MaxRows  int   `mapstructure:"max_rows"`
}

type DemoConfig struct {
	Seed bool `mapstructure:"seed"`
}

var logLevels = []string{"debug", "info", "warn", "error"}

func (c AppConfig) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}

	check(validAddr(c.HTTP.Addr), "http.addr must be host:port")
	check(validAddr(c.Metrics.Addr), "metrics.addr must be host:port")
	check(c.HTTP.Addr != c.Metrics.Addr, "metrics.addr must differ from http.addr")
	for name, d := range map[string]time.Duration{
		"http.read_header_timeout": c.HTTP.ReadHeaderTimeout,
		"http.read_timeout":        c.HTTP.ReadTimeout,
		"http.write_timeout":       c.HTTP.WriteTimeout,
		"http.idle_timeout":        c.HTTP.IdleTimeout,
		"http.shutdown_timeout":    c.HTTP.ShutdownTimeout,
		"health.timeout":           c.Health.Timeout,
		"database.connect_timeout": c.Database.ConnectTimeout,
	} {
		check(d > 0, "%s must be positive", name)
	}

	check(c.Database.Host != "", "database.host is required")
	check(c.Database.Port >= 1 && c.Database.Port <= 65535, "database.port must be between 1 and 65535")
	check(c.Database.Name != "", "database.name is required")
	check(c.Database.User != "", "database.user is required")
	check(!c.Database.Password.IsZero(), "database.password must not be empty")
	check(database.ValidSSLMode(c.Database.SSLMode), "database.ssl_mode must be one of disable, allow, prefer, require, verify-ca, verify-full")
	check(c.Database.MaxConns > 0, "database.max_conns must be greater than 0")

	check(slices.Contains(logLevels, c.Log.Level), "log.level must be one of debug, info, warn, error")

	check(c.Telemetry.ServiceName != "", "telemetry.service_name is required")
	tr := c.Telemetry.Tracing
	check(!tr.Enabled || tr.Endpoint != "", "telemetry.tracing.endpoint is required when tracing is enabled")
	check(tr.SamplingRate >= 0 && tr.SamplingRate <= 1, "telemetry.tracing.sampling_rate must be between 0 and 1")

	check(c.Import.MaxBytes > 0, "import.max_bytes must be greater than 0")
	check(c.Import.MaxRows > 0, "import.max_rows must be greater than 0")

	return errors.Join(errs...)
}

func validAddr(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && port != ""
}
