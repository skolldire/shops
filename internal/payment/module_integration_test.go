//go:build integration

package payment_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/payment"
	"github.com/skolldire/shops/internal/payment/paymenttest"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
)

func fake(t *testing.T) (*payment.Module, *pgxpool.Pool) {
	t.Helper()
	pool, _ := dbtest.Start(t)
	m, err := payment.NewModule(pool)
	require.NoError(t, err)
	return m, pool
}

func charges(t *testing.T, pool *pgxpool.Pool) func(string) int {
	return func(reference string) int {
		var n int
		require.NoError(t, pool.QueryRow(context.Background(),
			`SELECT count(*) FROM payment_fake_charges WHERE reference = $1 AND status <> 'VOIDED'`, reference).Scan(&n))
		return n
	}
}

func TestTheSimulatedProviderMeetsThePaymentContract(t *testing.T) {
	m, pool := fake(t)
	paymenttest.Contract(t, m, "pm_approved", charges(t, pool))
}

func TestTheSimulatedTokens(t *testing.T) {
	m, pool := fake(t)
	ctx := context.Background()
	charge := func(token string) (payment.ChargeResult, error) {
		return m.Charge(ctx, payment.ChargeRequest{Reference: uuid.NewString(), Token: token, Amount: decimal.RequireFromString("59.98"), Currency: "USD"})
	}

	approved, err := charge("pm_approved")
	require.NoError(t, err)
	require.Equal(t, payment.StatusApproved, approved.Status)
	require.True(t, strings.HasPrefix(approved.ProviderRef, "fake_"))
	other, err := charge("pm_approved")
	require.NoError(t, err)
	require.NotEqual(t, approved.ProviderRef, other.ProviderRef, "every charge has its own provider reference")

	for token, reason := range map[string]string{"pm_declined": payment.ReasonCardDeclined, "pm_whatever": payment.ReasonUnknownPaymentToken, "": payment.ReasonUnknownPaymentToken} {
		res, err := charge(token)
		require.NoError(t, err)
		require.Equal(t, payment.ChargeResult{Status: payment.StatusDeclined, Reason: reason}, res, token)
	}

	var tokens int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM payment_fake_charges WHERE token_hash LIKE 'pm_%'`).Scan(&tokens))
	require.Zero(t, tokens, "only the hash of the token is stored")
}

func TestAnUnknownOutcomeChargesAndThenAnswersTheStoredResult(t *testing.T) {
	m, pool := fake(t)
	ctx := context.Background()
	request := payment.ChargeRequest{Reference: uuid.NewString(), Token: "pm_unknown", Amount: decimal.RequireFromString("10.00"), Currency: "USD"}

	_, err := m.Charge(ctx, request)
	require.ErrorIs(t, err, payment.ErrOutcomeUnknown)
	require.Equal(t, 1, charges(t, pool)(request.Reference), "the charge happened although the answer was lost")

	again, err := m.Charge(ctx, request)
	require.NoError(t, err)
	require.Equal(t, payment.StatusApproved, again.Status)
	record, err := m.Lookup(ctx, request.Reference)
	require.NoError(t, err)
	require.Equal(t, again.ProviderRef, record.ProviderRef)
	require.Equal(t, 1, charges(t, pool)(request.Reference))
}

func TestTheProviderIsUnavailableWithoutItsDatabase(t *testing.T) {
	m, pool := fake(t)
	pool.Close()
	_, err := m.Charge(context.Background(), payment.ChargeRequest{Reference: "r", Token: "pm_approved", Amount: decimal.RequireFromString("1"), Currency: "USD"})
	require.ErrorIs(t, err, payment.ErrUnavailable)
	_, err = m.Lookup(context.Background(), "r")
	require.ErrorIs(t, err, payment.ErrUnavailable)
	_, err = m.Void(context.Background(), "r")
	require.ErrorIs(t, err, payment.ErrUnavailable)
}

func TestNewModuleRequiresAPool(t *testing.T) {
	_, err := payment.NewModule(nil)
	require.Error(t, err)
}
