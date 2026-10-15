package paymentgw

import (
	"context"
	"errors"
	"fmt"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/payment"
)

type Provider interface {
	Charge(ctx context.Context, r payment.ChargeRequest) (payment.ChargeResult, error)
}

type Gateway struct {
	provider Provider
}

func New(p Provider) (*Gateway, error) {
	if p == nil {
		return nil, errors.New("ordering: payments require a provider")
	}
	return &Gateway{provider: p}, nil
}

func (g *Gateway) Charge(ctx context.Context, c app.Charge) (app.ChargeResult, error) {
	res, err := g.provider.Charge(ctx, payment.ChargeRequest{Reference: c.Reference, Token: c.Token, Amount: c.Amount, Currency: c.Currency})
	switch {
	case errors.Is(err, payment.ErrUnavailable):
		return app.ChargeResult{}, fmt.Errorf("%w: %w", app.ErrPaymentUnavailable, err)
	case err != nil:
		return app.ChargeResult{}, fmt.Errorf("%w: %w", app.ErrPaymentOutcomeUnknown, err)
	}
	switch res.Status {
	case payment.StatusApproved:
		return app.ChargeResult{Approved: true, ProviderRef: res.ProviderRef}, nil
	case payment.StatusDeclined:
		return app.ChargeResult{Reason: res.Reason}, nil
	}
	return app.ChargeResult{}, fmt.Errorf("%w: unexpected status %q", app.ErrPaymentOutcomeUnknown, res.Status)
}
