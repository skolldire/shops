//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

type movementRow struct {
	Delta, StockAfter int
	Reason, Reference string
}

func (f fixture) movements(t *testing.T, id string) []movementRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT delta, stock_after, reason, COALESCE(reference, '<null>') FROM inventory_movements WHERE product_id = $1::uuid ORDER BY id`, id)
	require.NoError(t, err)
	defer rows.Close()
	var out []movementRow
	for rows.Next() {
		var m movementRow
		require.NoError(t, rows.Scan(&m.Delta, &m.StockAfter, &m.Reason, &m.Reference))
		out = append(out, m)
	}
	require.NoError(t, rows.Err())
	return out
}

func (f fixture) idOf(t *testing.T, sku string) string {
	t.Helper()
	var id string
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT id::text FROM products WHERE sku = $1`, sku).Scan(&id))
	return id
}

func (f fixture) requireBalanced(t *testing.T) {
	t.Helper()
	var unbalanced int
	require.NoError(t, f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM products p
		WHERE p.stock <> (SELECT COALESCE(sum(m.delta), 0) FROM inventory_movements m WHERE m.product_id = p.id)
		   OR p.reserved <> (SELECT COALESCE(sum(m.reserved_delta), 0) FROM inventory_movements m WHERE m.product_id = p.id)`).Scan(&unbalanced))
	require.Zero(t, unbalanced, "every product must satisfy stock = sum(delta) and reserved = sum(reserved_delta)")
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestInventoryAudit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	t.Run("create, edit and import record their reasons", func(t *testing.T) {
		f.reset(t)
		zero := f.create(t, "Z-1", "Zero", "Sports", "1.00", 0)
		require.Empty(t, f.movements(t, zero.ID), "a product created with stock 0 has no movement")

		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		_, err := f.repo.Update(ctx, a.ID, a.Version, domain.Changes{Name: sp("Alpha 2")})
		require.NoError(t, err)
		_, err = f.repo.Update(ctx, a.ID, a.Version+1, domain.Changes{Stock: ip(8)})
		require.NoError(t, err)

		_, err = f.repo.Upsert(ctx, []domain.Product{item("A-1", "Alpha 2", "10.00", 6), item("N-1", "New", "1.00", 4), item("N-0", "Empty", "1.00", 0)}, "sha-1")
		require.NoError(t, err)
		_, err = f.repo.Upsert(ctx, []domain.Product{item("A-1", "Alpha 2", "10.00", 6)}, "sha-2")
		require.NoError(t, err)

		require.Equal(t, []movementRow{
			{5, 5, "initial_stock", "<null>"},
			{3, 8, "manual_adjustment", "<null>"},
			{-2, 6, "import", "sha-1"},
		}, f.movements(t, a.ID), "renaming and reimporting an unchanged row add no movement")
		created := f.idOf(t, "N-1")
		require.Equal(t, []movementRow{{4, 4, "import", "sha-1"}}, f.movements(t, created), "a product created by import has a single import movement")
		require.Empty(t, f.movements(t, f.idOf(t, "N-0")))

		var unattributed int
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM inventory_movements WHERE reason = 'unattributed'`).Scan(&unattributed))
		require.Zero(t, unattributed)
		f.requireBalanced(t)
	})

	t.Run("a direct SQL update is recorded as unattributed with its database role", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		_, err := f.pool.Exec(ctx, `UPDATE products SET stock = 2 WHERE id = $1::uuid`, a.ID)
		require.NoError(t, err)
		var reason, user string
		var reference *string
		require.NoError(t, f.pool.QueryRow(ctx,
			`SELECT reason, reference, db_user FROM inventory_movements WHERE product_id = $1::uuid ORDER BY id DESC LIMIT 1`, a.ID).
			Scan(&reason, &reference, &user))
		require.Equal(t, "unattributed", reason)
		require.Nil(t, reference)
		require.Equal(t, "shop", user)
		f.requireBalanced(t)
	})

	t.Run("reserved units are recorded apart from the physical stock", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		change := func(reason, set string) error {
			tx, err := f.pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			_, err = tx.Exec(ctx, `SELECT set_config('shop.stock_reason', $1, true), set_config('shop.stock_reference', 'ref', true)`, reason)
			require.NoError(t, err)
			if _, err := tx.Exec(ctx, `UPDATE products SET `+set+` WHERE id = $1::uuid`, a.ID); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		require.NoError(t, change("reservation", "reserved = reserved + 3"))
		require.NoError(t, change("sale", "stock = stock - 2, reserved = reserved - 2"))
		require.NoError(t, change("reservation_release", "reserved = reserved - 1"))
		require.NoError(t, change("manual_adjustment", "reserved = reserved, name = 'Alpha 2'"))
		require.Equal(t, "23514", sqlState(change("reservation", "reserved = stock + 1")), "reserved never exceeds the stock")
		require.Equal(t, "23514", sqlState(change("reservation_release", "reserved = -1")))

		type row struct {
			Delta, StockAfter, ReservedDelta, ReservedAfter int
			Reason                                          string
		}
		rows, err := f.pool.Query(ctx,
			`SELECT delta, stock_after, reserved_delta, reserved_after, reason FROM inventory_movements WHERE product_id = $1::uuid ORDER BY id`, a.ID)
		require.NoError(t, err)
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
		require.NoError(t, err)
		require.Equal(t, []row{
			{5, 5, 0, 0, "initial_stock"},
			{0, 5, 3, 3, "reservation"},
			{-2, 3, -2, 1, "sale"},
			{0, 3, -1, 0, "reservation_release"},
		}, got, "an update that changes neither the stock nor the reserved units records nothing")
		f.requireBalanced(t)
	})

	t.Run("movements cannot be changed, deleted or truncated", func(t *testing.T) {
		f.reset(t)
		f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		for _, stmt := range []string{
			`UPDATE inventory_movements SET delta = 99`,
			`DELETE FROM inventory_movements`,
			`TRUNCATE inventory_movements`,
		} {
			_, err := f.pool.Exec(ctx, stmt)
			require.Error(t, err, stmt)
			require.Equal(t, "42501", sqlState(err), stmt)
		}
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM inventory_movements`).Scan(&n))
		require.Equal(t, 1, n)
	})

	t.Run("movements are listed newest first with paging", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		for _, ref := range []string{"p-1", "p-2", "p-3"} {
			_, err := f.repo.ReserveStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 1}}, ref)
			require.NoError(t, err)
		}
		items, total, err := f.repo.Movements(ctx, a.ID, domain.Paging{Page: 1, PageSize: 2})
		require.NoError(t, err)
		require.Equal(t, 4, total)
		require.Len(t, items, 2)
		require.Equal(t, "p-3", items[0].Reference)
		require.Equal(t, 5, items[0].StockAfter)
		require.Equal(t, []int{1, 3}, []int{items[0].ReservedDelta, items[0].ReservedAfter})
		require.Equal(t, "shop", items[0].DBUser)
		require.NotEmpty(t, items[0].TxID)
		items, _, err = f.repo.Movements(ctx, a.ID, domain.Paging{Page: 2, PageSize: 2})
		require.NoError(t, err)
		require.Equal(t, "p-1", items[0].Reference)
		require.Equal(t, "initial_stock", items[1].Reason)

		_, _, err = f.repo.Movements(ctx, "9b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b", domain.Paging{Page: 1, PageSize: 2})
		require.ErrorIs(t, err, app.ErrNotFound)
	})
}
