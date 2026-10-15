//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/postgres"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
)

func TestTxTranslatesRecoverableLockErrors(t *testing.T) {
	pool, _ := dbtest.Start(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE TABLE locks (id int PRIMARY KEY, n int NOT NULL); INSERT INTO locks VALUES (1, 0), (2, 0)`)
	require.NoError(t, err)
	manager, err := database.NewTxManager(pool)
	require.NoError(t, err)
	tx, err := postgres.NewTx(manager)
	require.NoError(t, err)

	t.Run("a deadlock is busy and rolled back", func(t *testing.T) {
		var (
			wg     sync.WaitGroup
			ready  sync.WaitGroup
			errs   = make([]error, 2)
			orders = [][2]int{{1, 2}, {2, 1}}
		)
		ready.Add(2)
		for i, order := range orders {
			wg.Go(func() {
				errs[i] = tx.WithinTx(ctx, func(ctx context.Context) error {
					q := database.Q(ctx, pool)
					if _, err := q.Exec(ctx, `UPDATE locks SET n = n + 1 WHERE id = $1`, order[0]); err != nil {
						ready.Done()
						return err
					}
					ready.Done()
					ready.Wait()
					_, err := q.Exec(ctx, `UPDATE locks SET n = n + 1 WHERE id = $1`, order[1])
					return err
				})
			})
		}
		wg.Wait()
		busy := 0
		for _, err := range errs {
			if err != nil {
				require.ErrorIs(t, err, app.ErrBusy)
				busy++
			}
		}
		require.Equal(t, 1, busy, "PostgreSQL aborts exactly one of them")
		var total int
		require.NoError(t, pool.QueryRow(ctx, `SELECT sum(n) FROM locks`).Scan(&total))
		require.Equal(t, 2, total, "only the surviving transaction applied its updates")
	})

	t.Run("a lock timeout is busy", func(t *testing.T) {
		holder, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = holder.Rollback(ctx) }()
		_, err = holder.Exec(ctx, `UPDATE locks SET n = n WHERE id = 1`)
		require.NoError(t, err)

		err = tx.WithinTx(ctx, func(ctx context.Context) error {
			q := database.Q(ctx, pool)
			if _, err := q.Exec(ctx, `SET LOCAL lock_timeout = '50ms'`); err != nil {
				return err
			}
			_, err := q.Exec(ctx, `UPDATE locks SET n = n + 1 WHERE id = 1`)
			return err
		})
		require.ErrorIs(t, err, app.ErrBusy)
	})

	t.Run("other errors are not busy", func(t *testing.T) {
		boom := errors.New("boom")
		err := tx.WithinTx(ctx, func(context.Context) error { return boom })
		require.ErrorIs(t, err, boom)
		require.NotErrorIs(t, err, app.ErrBusy)

		err = tx.WithinTx(ctx, func(ctx context.Context) error {
			_, err := database.Q(ctx, pool).Exec(ctx, `UPDATE locks SET n = -1 WHERE id = 99 RETURNING 1/0`)
			if err != nil {
				return err
			}
			_, err = database.Q(ctx, pool).Exec(ctx, `SELECT 1/0`)
			return err
		})
		require.Error(t, err)
		require.NotErrorIs(t, err, app.ErrBusy)
	})
}
