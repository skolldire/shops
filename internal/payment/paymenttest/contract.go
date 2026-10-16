package paymenttest

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/payment"
)

type Provider interface {
	Charge(ctx context.Context, r payment.ChargeRequest) (payment.ChargeResult, error)
	Lookup(ctx context.Context, reference string) (payment.Record, error)
	Void(ctx context.Context, reference string) (payment.Record, error)
}

func Contract(t *testing.T, p Provider, approveToken string, charges func(reference string) int) {
	ctx := context.Background()
	request := func(reference, amount string) payment.ChargeRequest {
		return payment.ChargeRequest{Reference: reference, Token: approveToken, Amount: decimal.RequireFromString(amount), Currency: "USD"}
	}

	t.Run("concurrent charges with the same reference charge once", func(t *testing.T) {
		reference := uuid.NewString()
		var (
			wg      sync.WaitGroup
			results = make([]payment.ChargeResult, 8)
			errs    = make([]error, 8)
		)
		for i := range results {
			wg.Go(func() { results[i], errs[i] = p.Charge(ctx, request(reference, "10.00")) })
		}
		wg.Wait()
		for i := range results {
			require.NoError(t, errs[i])
			require.Equal(t, results[0], results[i], "every request answers the one charge")
		}
		require.Equal(t, payment.StatusApproved, results[0].Status)
		require.Equal(t, 1, charges(reference))
		record, err := p.Lookup(ctx, reference)
		require.NoError(t, err)
		require.Equal(t, payment.StatusApproved, record.Status)
		require.Equal(t, results[0].ProviderRef, record.ProviderRef)
		require.True(t, record.Amount.Equal(decimal.RequireFromString("10.00")))
		require.Equal(t, "USD", record.Currency)
	})

	t.Run("repeating a charge with the same content returns the original result", func(t *testing.T) {
		reference := uuid.NewString()
		first, err := p.Charge(ctx, request(reference, "10.00"))
		require.NoError(t, err)
		again, err := p.Charge(ctx, request(reference, "10.00"))
		require.NoError(t, err)
		require.Equal(t, first, again)
		require.Equal(t, 1, charges(reference))
	})

	t.Run("repeating a charge with other content is a conflict without a new charge", func(t *testing.T) {
		reference := uuid.NewString()
		_, err := p.Charge(ctx, request(reference, "10.00"))
		require.NoError(t, err)
		_, err = p.Charge(ctx, request(reference, "11.00"))
		require.ErrorIs(t, err, payment.ErrReferenceConflict)
		require.Equal(t, 1, charges(reference))
		record, err := p.Lookup(ctx, reference)
		require.NoError(t, err)
		require.True(t, record.Amount.Equal(decimal.RequireFromString("10.00")), "the original charge is kept")
	})

	t.Run("an unknown reference is not found", func(t *testing.T) {
		record, err := p.Lookup(ctx, uuid.NewString())
		require.NoError(t, err)
		require.Equal(t, payment.StatusNotFound, record.Status)
	})

	t.Run("a voided reference can never be charged", func(t *testing.T) {
		reference := uuid.NewString()
		voided, err := p.Void(ctx, reference)
		require.NoError(t, err)
		require.Equal(t, payment.StatusVoided, voided.Status)
		late, err := p.Charge(ctx, request(reference, "10.00"))
		require.NoError(t, err)
		require.Equal(t, payment.ChargeResult{Status: payment.StatusDeclined, Reason: payment.ReasonVoided}, late)
		require.Zero(t, charges(reference))
		record, err := p.Lookup(ctx, reference)
		require.NoError(t, err)
		require.Equal(t, payment.StatusVoided, record.Status)
	})

	t.Run("voiding a charged reference returns the real result", func(t *testing.T) {
		reference := uuid.NewString()
		charged, err := p.Charge(ctx, request(reference, "10.00"))
		require.NoError(t, err)
		voided, err := p.Void(ctx, reference)
		require.NoError(t, err)
		require.Equal(t, payment.StatusApproved, voided.Status)
		require.Equal(t, charged.ProviderRef, voided.ProviderRef)
		require.Equal(t, 1, charges(reference))
	})
}
