package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/platform/validation"
)

func TestQueriesAreScopedToTheUser(t *testing.T) {
	f := setup(t, approve)
	first, err := f.run(t, f.standard())
	require.NoError(t, err)
	second, err := f.run(t, order(otherKey, "pm_approved", line(f.a, 1, "29.99")))
	require.NoError(t, err)
	q, err := app.NewQueries(f.w)
	require.NoError(t, err)
	ctx := context.Background()

	got, err := q.Purchase(ctx, "alice", string(first.Purchase.ID))
	require.NoError(t, err)
	require.Equal(t, first.Purchase, got)
	for _, raw := range []string{"nope", "urn:uuid:" + string(first.Purchase.ID), ""} {
		_, err = q.Purchase(ctx, "alice", raw)
		require.ErrorIs(t, err, app.ErrPurchaseNotFound)
	}
	_, err = q.Purchase(ctx, "bob", string(first.Purchase.ID))
	require.ErrorIs(t, err, app.ErrPurchaseNotFound)

	page, err := q.Purchases(ctx, "alice", nil, intp(1))
	require.NoError(t, err)
	require.Equal(t, 2, page.Total)
	require.Equal(t, 1, page.Page)
	require.Equal(t, second.Purchase.ID, page.Items[0].ID, "newest first")
	page, err = q.Purchases(ctx, "bob", nil, nil)
	require.NoError(t, err)
	require.Zero(t, page.Total)
	require.Equal(t, 20, page.PageSize)

	for _, size := range []int{0, 101} {
		_, err = q.Purchases(ctx, "alice", nil, intp(size))
		var errs validation.Errors
		require.ErrorAs(t, err, &errs, "page size %d", size)
	}
}

func intp(n int) *int {
	return &n
}
