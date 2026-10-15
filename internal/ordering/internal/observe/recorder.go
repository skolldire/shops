package observe

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/logger"
)

const (
	checkoutTotal = "checkout_total"
	recoveryTotal = "checkout_recovery_total"
	outcomeKey    = "outcome"
	component     = "ordering"
)

type Counters interface {
	Counter(ctx context.Context, name string, value int64, attrs ...attribute.KeyValue)
}

type CheckoutRecorder struct {
	counters Counters
	log      logger.Service
}

func NewCheckoutRecorder(counters Counters, log logger.Service) (*CheckoutRecorder, error) {
	if counters == nil || log == nil {
		return nil, errors.New("ordering: checkout recorder requires counters and a logger")
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

func (r *CheckoutRecorder) Recovery(ctx context.Context, outcome string) {
	r.counters.Counter(ctx, recoveryTotal, 1, attribute.String(outcomeKey, outcome))
}
