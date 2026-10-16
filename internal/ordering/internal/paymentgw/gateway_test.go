package paymentgw_test

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/paymentgw"
	"github.com/skolldire/shops/internal/payment"
)

type fakeProvider struct {
	res payment.ChargeResult
	err error
	got payment.ChargeRequest
}

func (f *fakeProvider) Charge(_ context.Context, r payment.ChargeRequest) (payment.ChargeResult, error) {
	f.got = r
	return f.res, f.err
}

func TestGatewayMapsResultsAndErrors(t *testing.T) {
	p := &fakeProvider{res: payment.ChargeResult{Status: payment.StatusApproved, ProviderRef: "fake_1"}}
	g, err := paymentgw.New(p)
	require.NoError(t, err)
	ctx := context.Background()
	charge := app.Charge{Reference: "1", Token: "pm_approved", Amount: decimal.RequireFromString("9.99"), Currency: "USD"}

	res, err := g.Charge(ctx, charge)
	require.NoError(t, err)
	require.Equal(t, app.ChargeResult{Approved: true, ProviderRef: "fake_1"}, res)
	require.Equal(t, payment.ChargeRequest{Reference: "1", Token: "pm_approved", Amount: charge.Amount, Currency: "USD"}, p.got)

	p.res = payment.ChargeResult{Status: payment.StatusDeclined, Reason: "card_declined"}
	res, err = g.Charge(ctx, charge)
	require.NoError(t, err)
	require.Equal(t, app.ChargeResult{Reason: "card_declined"}, res)

	p.err = payment.ErrUnavailable
	_, err = g.Charge(ctx, charge)
	require.ErrorIs(t, err, app.ErrPaymentUnavailable)

	for _, cause := range []error{payment.ErrOutcomeUnknown, errors.New("connection reset")} {
		p.err = cause
		_, err = g.Charge(ctx, charge)
		require.ErrorIs(t, err, app.ErrPaymentOutcomeUnknown)
	}

	p.err, p.res = nil, payment.ChargeResult{Status: "MAYBE"}
	_, err = g.Charge(ctx, charge)
	require.ErrorIs(t, err, app.ErrPaymentOutcomeUnknown)

	_, err = paymentgw.New(nil)
	require.Error(t, err)
}
