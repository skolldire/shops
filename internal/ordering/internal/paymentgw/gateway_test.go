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
	res    payment.ChargeResult
	record payment.Record
	err    error
	got    payment.ChargeRequest
	asked  []string
}

func (f *fakeProvider) Lookup(_ context.Context, reference string) (payment.Record, error) {
	f.asked = append(f.asked, "lookup "+reference)
	return f.record, f.err
}

func (f *fakeProvider) Void(_ context.Context, reference string) (payment.Record, error) {
	f.asked = append(f.asked, "void "+reference)
	return f.record, f.err
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

	p.err = payment.ErrReferenceConflict
	_, err = g.Charge(ctx, charge)
	require.ErrorIs(t, err, app.ErrPaymentOutcomeUnknown, "a conflict keeps the purchase pending")
	require.ErrorIs(t, err, app.ErrPaymentConflict)

	p.err, p.res = nil, payment.ChargeResult{Status: "MAYBE"}
	_, err = g.Charge(ctx, charge)
	require.ErrorIs(t, err, app.ErrPaymentOutcomeUnknown)

	_, err = paymentgw.New(nil)
	require.Error(t, err)
}

func TestGatewayTranslatesLookupAndVoid(t *testing.T) {
	p := &fakeProvider{}
	g, err := paymentgw.New(p)
	require.NoError(t, err)
	ctx := context.Background()
	amount := decimal.RequireFromString("9.99")

	for status, want := range map[payment.Status]app.PaymentStatus{
		payment.StatusApproved: app.PaymentApproved, payment.StatusDeclined: app.PaymentDeclined,
		payment.StatusVoided: app.PaymentVoided, payment.StatusNotFound: app.PaymentNotFound,
	} {
		p.record = payment.Record{Status: status, ProviderRef: "fake_1", Reason: "r", Amount: amount, Currency: "USD"}
		got, err := g.Lookup(ctx, "ref-1")
		require.NoError(t, err)
		require.Equal(t, app.PaymentRecord{Status: want, ProviderRef: "fake_1", Reason: "r", Amount: amount, Currency: "USD"}, got)
		got, err = g.Void(ctx, "ref-1")
		require.NoError(t, err)
		require.Equal(t, want, got.Status)
	}
	require.Equal(t, "lookup ref-1", p.asked[0])
	require.Equal(t, "void ref-1", p.asked[1])

	p.record = payment.Record{Status: "MAYBE"}
	_, err = g.Lookup(ctx, "ref-1")
	require.ErrorIs(t, err, app.ErrPaymentUnavailable)
	p.err = errors.New("timeout")
	_, err = g.Lookup(ctx, "ref-1")
	require.ErrorIs(t, err, app.ErrPaymentUnavailable, "a provider that does not answer leaves the purchase open")
	_, err = g.Void(ctx, "ref-1")
	require.ErrorIs(t, err, app.ErrPaymentUnavailable)
}
