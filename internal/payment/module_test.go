package payment_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/payment"
)

func charge(t *testing.T, token, reference string) payment.ChargeResult {
	t.Helper()
	res, err := payment.NewModule().Charge(context.Background(), payment.ChargeRequest{
		Reference: reference, Token: token, Amount: decimal.RequireFromString("59.98"), Currency: "USD",
	})
	require.NoError(t, err)
	return res
}

func TestFakeCharge(t *testing.T) {
	tests := map[string]struct {
		token string
		want  payment.ChargeResult
	}{
		"approved":      {"pm_approved", payment.ChargeResult{Status: payment.StatusApproved, ProviderRef: "fake_purchase-1"}},
		"declined":      {"pm_declined", payment.ChargeResult{Status: payment.StatusDeclined, Reason: payment.ReasonCardDeclined}},
		"unknown token": {"pm_whatever", payment.ChargeResult{Status: payment.StatusDeclined, Reason: payment.ReasonUnknownPaymentToken}},
		"empty token":   {"", payment.ChargeResult{Status: payment.StatusDeclined, Reason: payment.ReasonUnknownPaymentToken}},
		"case matters":  {"PM_APPROVED", payment.ChargeResult{Status: payment.StatusDeclined, Reason: payment.ReasonUnknownPaymentToken}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, charge(t, tc.token, "purchase-1"))
		})
	}
}

func TestFakeChargeIsDeterministicPerReference(t *testing.T) {
	first := charge(t, "pm_approved", "purchase-7")
	for range 3 {
		require.Equal(t, first, charge(t, "pm_approved", "purchase-7"))
	}
	require.NotEqual(t, first.ProviderRef, charge(t, "pm_approved", "purchase-8").ProviderRef)
	require.Equal(t, charge(t, "pm_declined", "purchase-7"), charge(t, "pm_declined", "purchase-7"))
}

func TestFakeChargeIgnoresCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := payment.NewModule().Charge(ctx, payment.ChargeRequest{Reference: "p", Token: "pm_approved"})
	require.NoError(t, err)
	require.Equal(t, payment.StatusApproved, res.Status)
}
