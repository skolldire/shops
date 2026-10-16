package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering"
	"github.com/skolldire/shops/internal/payment"
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
	api      http.Handler
	metrics  http.Handler
	catalog  *catalog.Module
	ordering *ordering.Module
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
	payments, err := payment.NewModule(pool)
	if err != nil {
		return nil, err
	}
	orders, err := ordering.NewModule(ordering.Deps{
		Pool: pool, Catalog: products, Payments: payments, Log: log, Metrics: tel, Users: cfg.Demo.Users,
		Budgets: ordering.Budgets{
			Reserve: cfg.Checkout.ReserveTimeout, Charge: cfg.Checkout.ChargeTimeout,
			Finalize: cfg.Checkout.FinalizeTimeout,
		},
		Recovery: ordering.RecoveryConfig{
			Interval: cfg.Recovery.Interval, BatchSize: cfg.Recovery.BatchSize, MinAge: cfg.Recovery.MinAge,
			Lease: cfg.Recovery.Lease, BaseBackoff: cfg.Recovery.BaseBackoff, MaxBackoff: cfg.Recovery.MaxBackoff,
			StuckAfterAttempts: cfg.Recovery.StuckAfterAttempts, MaxPendingAge: cfg.Checkout.MaxPendingAge,
		},
	})
	if err != nil {
		return nil, err
	}
	importBodyBytes := cfg.Import.MaxBytes + multipartOverheadBytes
	ui, err := web.New(web.Deps{Catalog: products, Ordering: orders, Users: cfg.Demo.Users, Log: log, MaxImportBodyBytes: importBodyBytes, PublicOrigins: cfg.HTTP.PublicOrigins})
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
			orders.RegisterRoutes(r)
		})
		api.Group(func(r chi.Router) {
			r.Use(httpx.MaxBodyBytes(importBodyBytes))
			products.RegisterImportRoutes(r)
		})
	})
	router.Mount("/", ui.Routes())

	metrics := http.NewServeMux()
	metrics.Handle("GET /metrics", tel.MetricsHandler())

	return &app{api: tel.HTTPMiddleware()(router), metrics: metrics, catalog: products, ordering: orders}, nil
}

func contextFields(ctx context.Context) map[string]any {
	fields := telemetry.TraceFields(ctx)
	if id := httpx.RequestIDFrom(ctx); id != "" {
		fields["request_id"] = id
	}
	return fields
}
