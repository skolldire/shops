//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/database"
)

type stockState struct {
	Stock, Reserved, Version int
}

func (f fixture) state(t *testing.T, id string) stockState {
	t.Helper()
	var s stockState
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT stock, reserved, version FROM products WHERE id = $1::uuid`, id).Scan(&s.Stock, &s.Reserved, &s.Version))
	return s
}

func (f fixture) reservedMovements(t *testing.T, id string) [][3]any {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT delta, reserved_delta, reason || ':' || COALESCE(reference, '') FROM inventory_movements
		 WHERE product_id = $1::uuid AND reason <> 'initial_stock' ORDER BY id`, id)
	require.NoError(t, err)
	defer rows.Close()
	var out [][3]any
	for rows.Next() {
		var delta, reserved int
		var label string
		require.NoError(t, rows.Scan(&delta, &reserved, &label))
		out = append(out, [3]any{delta, reserved, label})
	}
	require.NoError(t, rows.Err())
	return out
}

func TestReservations(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	req := func(p domain.Product, q int) []domain.StockRequest {
		return []domain.StockRequest{{ProductID: p.ID, Quantity: q}}
	}

	t.Run("a reservation keeps the stock and the version and returns the snapshot", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "29.99", 5)
		lines, err := f.repo.ReserveStock(ctx, req(a, 3), "purchase-1")
		require.NoError(t, err)
		require.Equal(t, []domain.StockLine{{ProductID: a.ID, SKU: "A-1", Name: "Alpha", Price: d("29.99"), Quantity: 3}}, lines)
		require.Equal(t, stockState{5, 3, 1}, f.state(t, a.ID))
		require.Equal(t, [][3]any{{0, 3, "reservation:purchase-1"}}, f.reservedMovements(t, a.ID))
		f.requireBalanced(t)
	})

	t.Run("a reservation counts only the available units", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "1.00", 5)
		_, err := f.repo.ReserveStock(ctx, req(a, 3), "purchase-1")
		require.NoError(t, err)
		_, err = f.repo.ReserveStock(ctx, req(a, 3), "purchase-2")
		var short *app.InsufficientStockError
		require.ErrorAs(t, err, &short)
		require.Equal(t, 2, short.Available)
		require.Equal(t, stockState{5, 3, 1}, f.state(t, a.ID))
	})

	t.Run("a deleted product cannot be reserved but settles its reservations", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "1.00", 5)
		_, err := f.repo.ReserveStock(ctx, req(a, 3), "purchase-1")
		require.NoError(t, err)
		require.NoError(t, f.repo.Delete(ctx, a.ID, a.Version))
		_, err = f.repo.ReserveStock(ctx, req(a, 1), "purchase-2")
		require.ErrorIs(t, err, app.ErrProductUnavailable)

		require.NoError(t, f.repo.ReleaseStock(ctx, req(a, 1), "purchase-1"))
		require.NoError(t, f.repo.CommitStock(ctx, req(a, 2), "purchase-1"))
		require.Equal(t, stockState{3, 0, 3}, f.state(t, a.ID), "only the commit bumps the version, after the delete")
		require.Equal(t, [][3]any{
			{0, 3, "reservation:purchase-1"},
			{0, -1, "reservation_release:purchase-1"},
			{-2, -2, "sale:purchase-1"},
		}, f.reservedMovements(t, a.ID))
		f.requireBalanced(t)
	})

	t.Run("settling more than is reserved is an invariant error and changes nothing", func(t *testing.T) {
		f.reset(t)
		a, b := f.create(t, "A-1", "Alpha", "Sports", "1.00", 5), f.create(t, "B-1", "Beta", "Sports", "1.00", 5)
		_, err := f.repo.ReserveStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 2}, {ProductID: b.ID, Quantity: 1}}, "purchase-1")
		require.NoError(t, err)
		both := []domain.StockRequest{{ProductID: a.ID, Quantity: 2}, {ProductID: b.ID, Quantity: 2}}
		for name, settle := range map[string]func() error{
			"release": func() error { return f.repo.ReleaseStock(ctx, both, "purchase-1") },
			"commit":  func() error { return f.repo.CommitStock(ctx, both, "purchase-1") },
		} {
			err := settle()
			require.ErrorIs(t, err, app.ErrStockInvariant, name)
			var invariant *app.StockInvariantError
			require.ErrorAs(t, err, &invariant)
			require.Equal(t, b.ID, invariant.ProductID)
			require.Equal(t, stockState{5, 2, 1}, f.state(t, a.ID), name)
			require.Equal(t, stockState{5, 1, 1}, f.state(t, b.ID), name)
		}
		f.requireBalanced(t)
	})

	t.Run("a short item rolls back the reservations before it", func(t *testing.T) {
		f.reset(t)
		a, b := f.create(t, "A-1", "Alpha", "Sports", "1.00", 5), f.create(t, "B-1", "Beta", "Sports", "1.00", 1)
		_, err := f.repo.ReserveStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 2}, {ProductID: b.ID, Quantity: 2}}, "purchase-1")
		var short *app.InsufficientStockError
		require.ErrorAs(t, err, &short)
		require.Equal(t, b.ID, short.ProductID)
		require.Equal(t, 1, short.Available)
		require.Equal(t, stockState{5, 0, 1}, f.state(t, a.ID))
		require.Empty(t, f.reservedMovements(t, a.ID))

		_, err = f.repo.ReserveStock(ctx, []domain.StockRequest{{ProductID: "9b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b", Quantity: 1}}, "purchase-2")
		var unavailable *app.ProductUnavailableError
		require.ErrorAs(t, err, &unavailable)
		require.Equal(t, "9b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b", unavailable.ProductID)
	})

	t.Run("a writer without reference does not inherit the previous one in the same transaction", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "1.00", 5)
		tm, err := database.NewTxManager(f.pool)
		require.NoError(t, err)
		require.NoError(t, tm.WithinTx(ctx, func(ctx context.Context) error {
			if _, err := f.repo.ReserveStock(ctx, req(a, 1), "purchase-1"); err != nil {
				return err
			}
			_, err := f.repo.Update(ctx, a.ID, a.Version, domain.Changes{Stock: ip(9)})
			return err
		}))
		require.Equal(t, [][3]any{{0, 1, "reservation:purchase-1"}, {4, 0, "manual_adjustment:"}}, f.reservedMovements(t, a.ID),
			"the reservation did not change the version the edit was based on")
	})

	t.Run("the operations run in the caller's transaction", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "1.00", 5)
		tm, err := database.NewTxManager(f.pool)
		require.NoError(t, err)
		rollback := errors.New("rollback")
		err = tm.WithinTx(ctx, func(ctx context.Context) error {
			if _, err := f.repo.ReserveStock(ctx, req(a, 2), "purchase-1"); err != nil {
				return err
			}
			if err := f.repo.CommitStock(ctx, req(a, 2), "purchase-1"); err != nil {
				return err
			}
			return rollback
		})
		require.ErrorIs(t, err, rollback)
		require.Equal(t, stockState{5, 0, 1}, f.state(t, a.ID))
		require.Empty(t, f.reservedMovements(t, a.ID))
	})
}
