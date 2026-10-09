//go:build integration

package database_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/secret"
)

func startPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	schema, err := filepath.Abs(filepath.Join("..", "..", "..", "db", "init", "001_schema.sql"))
	require.NoError(t, err)

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("shop"),
		tcpostgres.WithUsername("shop"),
		tcpostgres.WithPassword("integration-password"),
		tcpostgres.WithInitScripts(schema),
		tcpostgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	require.NoError(t, err)

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	portNum, err := strconv.Atoi(port.Port())
	require.NoError(t, err)

	pool, err := database.Open(ctx, database.Config{
		Host: host, Port: portNum, Name: "shop", User: "shop",
		Password: secret.New("integration-password"), SSLMode: "disable",
		MaxConns: 4, ConnectTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func insertProduct(ctx context.Context, q database.Querier, sku string, stock int) error {
	_, err := q.Exec(ctx,
		`INSERT INTO products (sku, name, category, price, stock, weight_kg) VALUES ($1, $2, 'test', 10.00, $3, 1.0)`,
		sku, "Product "+sku, stock)
	return err
}

func countProducts(t *testing.T, pool *pgxpool.Pool, sku string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM products WHERE sku = $1`, sku).Scan(&n))
	return n
}

func TestDatabase(t *testing.T) {
	pool := startPostgres(t)
	tm, err := database.NewTxManager(pool)
	require.NoError(t, err)
	ctx := context.Background()

	t.Run("schema has four tables", func(t *testing.T) {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name IN ('products','purchases','purchase_items','cart_items')`).Scan(&n))
		require.Equal(t, 4, n)
	})

	t.Run("checker", func(t *testing.T) {
		c, err := database.NewChecker(pool)
		require.NoError(t, err)
		require.NoError(t, c.Check(ctx))
	})

	t.Run("commit", func(t *testing.T) {
		err := tm.WithinTx(ctx, func(ctx context.Context) error {
			if err := insertProduct(ctx, database.Q(ctx, pool), "COMMIT-1", 5); err != nil {
				return err
			}
			return insertProduct(ctx, database.Q(ctx, pool), "COMMIT-2", 5)
		})
		require.NoError(t, err)
		require.Equal(t, 2, countProducts(t, pool, "COMMIT-1")+countProducts(t, pool, "COMMIT-2"))
	})

	t.Run("rollback on error", func(t *testing.T) {
		boom := errors.New("boom")
		err := tm.WithinTx(ctx, func(ctx context.Context) error {
			require.NoError(t, insertProduct(ctx, database.Q(ctx, pool), "ROLLBACK-ERR", 5))
			return boom
		})
		require.ErrorIs(t, err, boom)
		require.Zero(t, countProducts(t, pool, "ROLLBACK-ERR"))
	})

	t.Run("rollback on panic", func(t *testing.T) {
		require.PanicsWithValue(t, "kaboom", func() {
			_ = tm.WithinTx(ctx, func(ctx context.Context) error {
				require.NoError(t, insertProduct(ctx, database.Q(ctx, pool), "ROLLBACK-PANIC", 5))
				panic("kaboom")
			})
		})
		require.Zero(t, countProducts(t, pool, "ROLLBACK-PANIC"))
	})

	t.Run("nested call reuses the outer transaction", func(t *testing.T) {
		boom := errors.New("outer fails")
		err := tm.WithinTx(ctx, func(ctx context.Context) error {
			outer := database.Q(ctx, pool)
			require.NoError(t, tm.WithinTx(ctx, func(ctx context.Context) error {
				require.Same(t, outer, database.Q(ctx, pool))
				return insertProduct(ctx, database.Q(ctx, pool), "NESTED", 5)
			}))
			return boom
		})
		require.ErrorIs(t, err, boom)
		require.Zero(t, countProducts(t, pool, "NESTED"))
	})

	t.Run("Q returns the pool outside and the tx inside", func(t *testing.T) {
		require.IsType(t, &pgxpool.Pool{}, database.Q(ctx, pool))
		require.NoError(t, tm.WithinTx(ctx, func(ctx context.Context) error {
			require.Implements(t, (*pgx.Tx)(nil), database.Q(ctx, pool))
			return nil
		}))
	})

	t.Run("stock check constraint", func(t *testing.T) {
		require.NoError(t, insertProduct(ctx, pool, "STOCK-1", 2))
		_, err := pool.Exec(ctx, `UPDATE products SET stock = stock - 3 WHERE sku = 'STOCK-1'`)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, "23514", pgErr.Code)
	})
}
