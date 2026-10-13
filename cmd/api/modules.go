package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/health"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
	"github.com/skolldire/shops/internal/web"
)

const (
	maxRequestBytes        = 1 << 20
	multipartOverheadBytes = 64 << 10
)

type app struct {
	api     http.Handler
	metrics http.Handler
	catalog *catalog.Module
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

	limits := catalog.ImportLimits{MaxBytes: cfg.Import.MaxBytes, MaxRows: cfg.Import.MaxRows}
	products, err := catalog.NewModule(catalog.Deps{Pool: pool, Log: log, Metrics: tel, Tracer: tel, Import: limits})
	if err != nil {
		return nil, err
	}
	importBodyBytes := cfg.Import.MaxBytes + multipartOverheadBytes
	ui, err := web.New(web.Deps{Catalog: products, Log: log, MaxImportBodyBytes: importBodyBytes})
	if err != nil {
		return nil, err
	}

	router := httpx.NewRouter(log)
	router.Route("/health", func(r chi.Router) {
		r.Get("/live", healthSvc.Live)
		r.Get("/ready", healthSvc.Ready)
	})
	router.Route("/api/v1", func(api chi.Router) {
		api.Group(func(r chi.Router) {
			r.Use(httpx.MaxBodyBytes(maxRequestBytes))
			products.RegisterRoutes(r)
		})
		api.Group(func(r chi.Router) {
			r.Use(httpx.MaxBodyBytes(importBodyBytes))
			products.RegisterImportRoutes(r)
		})
	})
	router.Mount("/", ui.Routes())

	metrics := http.NewServeMux()
	metrics.Handle("GET /metrics", tel.MetricsHandler())

	return &app{api: tel.HTTPMiddleware()(router), metrics: metrics, catalog: products}, nil
}

func contextFields(ctx context.Context) map[string]any {
	fields := telemetry.TraceFields(ctx)
	if id := httpx.RequestIDFrom(ctx); id != "" {
		fields["request_id"] = id
	}
	return fields
}
