package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/health"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

type app struct {
	api     http.Handler
	metrics http.Handler
}

func buildApp(cfg AppConfig, log logger.Service, pool *pgxpool.Pool, tel *telemetry.Provider) (*app, error) {
	dbChecker, err := database.NewChecker(pool)
	if err != nil {
		return nil, err
	}
	healthSvc, err := health.New(cfg.Health, log)
	if err != nil {
		return nil, err
	}
	healthSvc.Register("postgres", dbChecker)

	router := httpx.NewRouter(log)
	router.Get("/health/live", healthSvc.Live)
	router.Get("/health/ready", healthSvc.Ready)
	router.Mount("/api", chi.NewRouter())

	metrics := http.NewServeMux()
	metrics.Handle("GET /metrics", tel.MetricsHandler())

	return &app{api: tel.HTTPMiddleware()(router), metrics: metrics}, nil
}

func contextFields(ctx context.Context) map[string]any {
	fields := telemetry.TraceFields(ctx)
	if id := httpx.RequestIDFrom(ctx); id != "" {
		fields["request_id"] = id
	}
	return fields
}
