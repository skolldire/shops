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
	"github.com/skolldire/shops/internal/platform/database"
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

func (f fixture) stockOf(t *testing.T, id string) (stock, version int) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT stock, version FROM products WHERE id = $1::uuid`, id).Scan(&stock, &version))
	return stock, version
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

func TestStock(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	t.Run("take stock decrements, bumps the version and returns the snapshot", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		b := f.create(t, "B-1", "Beta", "Sports", "2.50", 3)
		lines, err := f.repo.TakeStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 2}, {ProductID: b.ID, Quantity: 3}}, "purchase-1")
		require.NoError(t, err)
		require.Len(t, lines, 2)
		require.Equal(t, "A-1", lines[0].SKU)
		require.Equal(t, "Alpha", lines[0].Name)
		require.True(t, d("10.00").Equal(lines[0].Price))
		require.Equal(t, 2, lines[0].Quantity)
		stock, version := f.stockOf(t, a.ID)
		require.Equal(t, 3, stock)
		require.Equal(t, a.Version+1, version)
		stock, _ = f.stockOf(t, b.ID)
		require.Zero(t, stock)
		require.Equal(t, []movementRow{{5, 5, "initial_stock", "<null>"}, {-2, 3, "sale", "purchase-1"}}, f.movements(t, a.ID))
		f.requireBalanced(t)
	})

	t.Run("insufficient stock reports what is available and changes nothing", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		b := f.create(t, "B-1", "Beta", "Sports", "2.50", 1)
		_, err := f.repo.TakeStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 2}, {ProductID: b.ID, Quantity: 2}}, "purchase-1")
		var short *app.InsufficientStockError
		require.ErrorAs(t, err, &short)
		require.Equal(t, b.ID, short.ProductID)
		require.Equal(t, 1, short.Available)
		stock, version := f.stockOf(t, a.ID)
		require.Equal(t, 5, stock, "the decrement of the first item is rolled back")
		require.Equal(t, a.Version, version)
		require.Len(t, f.movements(t, a.ID), 1)
	})

	t.Run("a deleted or unknown product is unavailable", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		require.NoError(t, f.repo.Delete(ctx, a.ID, a.Version))
		_, err := f.repo.TakeStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 1}}, "purchase-1")
		require.ErrorIs(t, err, app.ErrProductUnavailable)
		var unavailable *app.ProductUnavailableError
		require.ErrorAs(t, err, &unavailable)
		require.Equal(t, a.ID, unavailable.ProductID)
		_, err = f.repo.TakeStock(ctx, []domain.StockRequest{{ProductID: "9b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b", Quantity: 1}}, "purchase-1")
		require.ErrorIs(t, err, app.ErrProductUnavailable)
	})

	t.Run("return stock ignores the status and does not reactivate", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		_, err := f.repo.TakeStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 2}}, "purchase-1")
		require.NoError(t, err)
		_, version := f.stockOf(t, a.ID)
		require.NoError(t, f.repo.Delete(ctx, a.ID, version))
		require.NoError(t, f.repo.ReturnStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 2}}, "purchase-1"))
		stock, after := f.stockOf(t, a.ID)
		require.Equal(t, 5, stock)
		require.Equal(t, version+2, after)
		var status string
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT status FROM products WHERE id = $1::uuid`, a.ID).Scan(&status))
		require.Equal(t, "DELETED", status)
		require.Equal(t, movementRow{2, 5, "sale_reversal", "purchase-1"}, f.movements(t, a.ID)[2])
		f.requireBalanced(t)
	})

	t.Run("return stock fails for an unknown product or an overflow", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 2147483647)
		require.Error(t, f.repo.ReturnStock(ctx, []domain.StockRequest{{ProductID: "9b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b", Quantity: 1}}, "p"))
		err := f.repo.ReturnStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 1}}, "p")
		require.Equal(t, "22003", sqlState(err))
	})

	t.Run("a rollback of the caller's transaction undoes the decrement and its movement", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		tx, err := database.NewTxManager(f.pool)
		require.NoError(t, err)
		rollback := errors.New("caller rolls back")
		err = tx.WithinTx(ctx, func(ctx context.Context) error {
			if _, err := f.repo.TakeStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 4}}, "purchase-1"); err != nil {
				return err
			}
			return rollback
		})
		require.ErrorIs(t, err, rollback)
		stock, version := f.stockOf(t, a.ID)
		require.Equal(t, 5, stock)
		require.Equal(t, a.Version, version)
		require.Len(t, f.movements(t, a.ID), 1)
	})

	t.Run("a writer without reference does not inherit the previous one in the same transaction", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Sports", "10.00", 5)
		tx, err := database.NewTxManager(f.pool)
		require.NoError(t, err)
		require.NoError(t, tx.WithinTx(ctx, func(ctx context.Context) error {
			if _, err := f.repo.TakeStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 1}}, "purchase-1"); err != nil {
				return err
			}
			_, err := f.repo.Update(ctx, a.ID, a.Version+1, domain.Changes{Stock: ip(9)})
			return err
		}))
		got := f.movements(t, a.ID)
		require.Equal(t, movementRow{-1, 4, "sale", "purchase-1"}, got[1])
		require.Equal(t, movementRow{5, 9, "manual_adjustment", "<null>"}, got[2])
	})
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
			_, err := f.repo.TakeStock(ctx, []domain.StockRequest{{ProductID: a.ID, Quantity: 1}}, ref)
			require.NoError(t, err)
		}
		items, total, err := f.repo.Movements(ctx, a.ID, domain.Paging{Page: 1, PageSize: 2})
		require.NoError(t, err)
		require.Equal(t, 4, total)
		require.Len(t, items, 2)
		require.Equal(t, "p-3", items[0].Reference)
		require.Equal(t, 2, items[0].StockAfter)
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
