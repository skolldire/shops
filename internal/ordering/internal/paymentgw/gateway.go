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
	Lookup(ctx context.Context, reference string) (payment.Record, error)
	Void(ctx context.Context, reference string) (payment.Record, error)
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
	case errors.Is(err, payment.ErrReferenceConflict):
		return app.ChargeResult{}, fmt.Errorf("%w: %w", app.ErrPaymentOutcomeUnknown, app.ErrPaymentConflict)
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

func (g *Gateway) Lookup(ctx context.Context, reference string) (app.PaymentRecord, error) {
	rec, err := g.provider.Lookup(ctx, reference)
	return translate(rec, err)
}

func (g *Gateway) Void(ctx context.Context, reference string) (app.PaymentRecord, error) {
	rec, err := g.provider.Void(ctx, reference)
	return translate(rec, err)
}

func translate(rec payment.Record, err error) (app.PaymentRecord, error) {
	if err != nil {
		return app.PaymentRecord{}, fmt.Errorf("%w: %w", app.ErrPaymentUnavailable, err)
	}
	out := app.PaymentRecord{ProviderRef: rec.ProviderRef, Reason: rec.Reason, Amount: rec.Amount, Currency: rec.Currency}
	switch rec.Status {
	case payment.StatusApproved:
		out.Status = app.PaymentApproved
	case payment.StatusDeclined:
		out.Status = app.PaymentDeclined
	case payment.StatusVoided:
		out.Status = app.PaymentVoided
	case payment.StatusNotFound:
		out.Status = app.PaymentNotFound
	default:
		return app.PaymentRecord{}, fmt.Errorf("%w: unexpected status %q", app.ErrPaymentUnavailable, rec.Status)
	}
	return out, nil
}
