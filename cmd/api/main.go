package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/skolldire/shops/internal/platform/config"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/lifecycle"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

const defaultConfigPath = "/etc/shop/config.yaml"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		report(os.Stderr, err)
		os.Exit(1)
	}
}

type loggedError struct {
	err error
}

func (e *loggedError) Error() string { return e.err.Error() }

func (e *loggedError) Unwrap() error { return e.err }

func report(w io.Writer, err error) {
	var logged *loggedError
	if errors.As(err, &logged) {
		return
	}
	_, _ = fmt.Fprintln(w, err)
}

func run(args []string, stdout io.Writer) error {
	if len(args) > 0 && args[0] == "healthcheck" {
		return runHealthcheck(args[1:])
	}

	flags := flag.NewFlagSet("api", flag.ContinueOnError)
	configFlag := flags.String("config", "", "path to the YAML configuration file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	configPath := resolveConfigPath(*configFlag, os.Getenv("CONFIG_FILE"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load[AppConfig](ctx, configPath, config.Resolvers{
		"env":  config.Env(os.LookupEnv),
		"file": config.File(os.ReadFile),
	})
	if err != nil {
		return err
	}

	log, err := logger.New(cfg.Log, stdout, logger.WithContextExtractor(contextFields))
	if err != nil {
		return err
	}
	log.Info(ctx, "application starting", map[string]any{"config_path": configPath})

	lc, err := lifecycle.New(log)
	if err != nil {
		return err
	}
	if err := start(ctx, cfg, log, lc); err != nil {
		log.Error(ctx, err, nil)
		return &loggedError{err: errors.Join(err, lc.Close(context.WithoutCancel(ctx)))}
	}
	return nil
}

func start(ctx context.Context, cfg AppConfig, log logger.Service, lc *lifecycle.Lifecycle) error {
	tel, err := telemetry.New(ctx, cfg.Telemetry, log)
	if err != nil {
		return err
	}
	if err := lc.OnClose("telemetry", tel.Shutdown); err != nil {
		return err
	}

	pool, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	if err := lc.OnClose("postgres", func(context.Context) error { pool.Close(); return nil }); err != nil {
		pool.Close()
		return err
	}
	log.Info(ctx, "database connected", nil)

	a, err := buildApp(cfg, log, pool, tel)
	if err != nil {
		return err
	}

	apiServer := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           a.api,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}
	metricsServer := &http.Server{
		Addr:              cfg.Metrics.Addr,
		Handler:           a.metrics,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return lc.Serve(ctx, cfg.HTTP.ShutdownTimeout, apiServer, metricsServer)
}

func resolveConfigPath(flagValue, envValue string) string {
	switch {
	case flagValue != "":
		return flagValue
	case envValue != "":
		return envValue
	default:
		return defaultConfigPath
	}
}
