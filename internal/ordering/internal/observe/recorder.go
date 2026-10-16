package observe

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/logger"
)

const (
	checkoutTotal      = "checkout_total"
	recoveryTotal      = "checkout_recovery_total"
	resolutionsTotal   = "checkout_recovery_resolutions_total"
	pendingGauge       = "checkout_pending_purchases"
	oldestPendingGauge = "checkout_oldest_pending_seconds"
	stuckGauge         = "checkout_recovery_stuck"
	stuckReservedGauge = "checkout_recovery_stuck_reserved_units"
	outcomeKey         = "outcome"
	resultKey          = "result"
	component          = "ordering"
)

type Counters interface {
	Counter(ctx context.Context, name string, value int64, attrs ...attribute.KeyValue)
	Gauge(ctx context.Context, name string, value float64, attrs ...attribute.KeyValue)
}

type CheckoutRecorder struct {
	counters Counters
	log      logger.Service
}

func NewCheckoutRecorder(counters Counters, log logger.Service) (*CheckoutRecorder, error) {
	if counters == nil || log == nil {
		return nil, errors.New("ordering: checkout recorder requires counters and a logger")
	}
	ctx := context.Background()
	for _, outcome := range []string{app.RecoveryConfirmed, app.RecoveryDeclined, app.RecoveryStillPending, app.RecoveryError, app.RecoverySkipped} {
		counters.Counter(ctx, recoveryTotal, 0, attribute.String(outcomeKey, outcome))
	}
	for _, result := range []string{app.ResolutionConfirmed, app.ResolutionDeclined, app.ResolutionVoided, app.ResolutionMismatch, app.ResolutionError} {
		counters.Counter(ctx, resolutionsTotal, 0, attribute.String(resultKey, result))
	}
	return &CheckoutRecorder{counters: counters, log: log}, nil
}

func (r *CheckoutRecorder) Checkout(ctx context.Context, outcome string, p *domain.Purchase, cause error) {
	r.counters.Counter(ctx, checkoutTotal, 1, attribute.String(outcomeKey, outcome))
	if p == nil {
		return
	}
	fields := map[string]any{
		"component": component, "outcome": outcome,
		"purchase_id": string(p.ID), "user_id": p.UserID, "status": string(p.Status),
	}
	if cause != nil {
		fields["cause"] = cause.Error()
		r.log.Warn(ctx, "ordering: checkout finished without a definitive payment result", fields)
		return
	}
	r.log.Info(ctx, "ordering: checkout finished", fields)
}

func (r *CheckoutRecorder) RecoveryAttempt(ctx context.Context, a app.RecoveryAttempt) {
	r.counters.Counter(ctx, recoveryTotal, 1, attribute.String(outcomeKey, a.Outcome))
	fields := map[string]any{"component": component, "purchase_id": string(a.PurchaseID), "attempt": a.Attempts, "outcome": a.Outcome}
	if a.Cause != nil {
		fields["cause"] = a.Cause.Error()
	}
	switch {
	case a.Outcome == app.RecoverySkipped:
		r.log.Info(ctx, "ordering: recovery skipped a purchase another worker took or settled", fields)
	case a.Outcome == app.RecoveryConfirmed || a.Outcome == app.RecoveryDeclined:
		if a.Attempts >= a.StuckAfter {
			r.log.Info(ctx, "ordering: a stuck purchase was resolved", fields)
			return
		}
		r.log.Info(ctx, "ordering: recovery settled a purchase", fields)
	case a.Attempts == a.StuckAfter:
		r.log.Error(ctx, errStuck, fields)
	case a.Attempts > a.StuckAfter:
		r.log.Warn(ctx, "ordering: a stuck purchase is still pending", fields)
	default:
		r.log.Info(ctx, "ordering: recovery left a purchase pending", fields)
	}
}

var errStuck = errors.New("ordering: a purchase is stuck in recovery and is resolved by lookup or void from now on")

func (r *CheckoutRecorder) RecoveryResolution(ctx context.Context, res app.RecoveryResolution) {
	r.counters.Counter(ctx, resolutionsTotal, 1, attribute.String(resultKey, res.Result))
	fields := map[string]any{"component": component, "purchase_id": string(res.PurchaseID), "attempts": res.Attempts, "result": res.Result}
	if res.Cause != nil {
		fields["cause"] = res.Cause.Error()
	}
	if res.Result == app.ResolutionMismatch {
		r.log.Error(ctx, errMismatch, fields)
		return
	}
	r.log.Info(ctx, "ordering: recovery resolved a purchase with the provider", fields)
}

var errMismatch = errors.New("ordering: the provider charged another amount or currency, the purchase stays pending")

func (r *CheckoutRecorder) RecoveryGauges(ctx context.Context, s app.PendingStats) {
	r.counters.Gauge(ctx, pendingGauge, float64(s.Pending))
	r.counters.Gauge(ctx, oldestPendingGauge, s.OldestSeconds)
	r.counters.Gauge(ctx, stuckGauge, float64(s.Stuck))
	r.counters.Gauge(ctx, stuckReservedGauge, float64(s.StuckReservedUnits))
}
