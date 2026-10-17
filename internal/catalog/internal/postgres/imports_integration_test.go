//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/catalog/internal/postgres"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
)

const importKey = "6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"

func applying(key, hash string, mode domain.ImportMode) app.ImportRecord {
	return app.ImportRecord{Key: key, Mode: mode, Status: domain.ImportApplying, File: app.ImportFile{Name: "stock.csv", Bytes: 3, SHA256: hash}}
}

func TestImportLog(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	log, err := postgres.NewImportLog(f.pool)
	require.NoError(t, err)
	reset := func(t *testing.T) { t.Helper(); dbtest.Truncate(t, f.pool, "imports") }

	t.Run("a key returns the import it created", func(t *testing.T) {
		reset(t)
		rec, created, err := log.Create(ctx, applying(importKey, "h1", domain.ImportReplace))
		require.NoError(t, err)
		require.True(t, created)
		require.Equal(t, domain.ImportApplying, rec.Status)
		require.Equal(t, app.ImportFile{Name: "stock.csv", Bytes: 3, SHA256: "h1"}, rec.File)

		again, created, err := log.Create(ctx, applying(importKey, "other", domain.ImportAdd))
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, rec.ID, again.ID)
		require.Equal(t, "h1", again.File.SHA256, "the stored import answers, not the new request")

		_, err = log.Get(ctx, "9b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b")
		require.ErrorIs(t, err, app.ErrImportNotFound)
	})

	t.Run("an import finishes once and keeps its report", func(t *testing.T) {
		reset(t)
		rec, _, err := log.Create(ctx, applying("", "h1", domain.ImportReplace))
		require.NoError(t, err)
		report := app.ImportReport{Records: 3, Created: 1, Updated: 1, Rejected: 1,
			Errors:  []app.RowError{{Line: 4, SKU: "B-1", Field: "stock", Code: "below_reserved", Message: "m"}},
			Changes: []domain.ImportChange{{SKU: "A-1", StockBefore: 50, StockAfter: 10, PriceBefore: d("29.99"), PriceAfter: d("39.99")}},
		}
		ok, err := log.Finish(ctx, rec.ID, domain.ImportApplying, report)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = log.Finish(ctx, rec.ID, domain.ImportApplying, report)
		require.NoError(t, err)
		require.False(t, ok, "only an import still applying can finish")

		applied, err := log.Get(ctx, rec.ID)
		require.NoError(t, err)
		require.Equal(t, domain.ImportApplied, applied.Status)
		require.False(t, applied.Stamp.AppliedAt.IsZero())
		require.Equal(t, report.Errors, applied.Report.Errors)
		require.Len(t, applied.Report.Changes, 1)
		require.True(t, applied.Report.Changes[0].PriceAfter.Equal(d("39.99")))
		require.Equal(t, []int{3, 1, 1, 1}, []int{applied.Report.Records, applied.Report.Created, applied.Report.Updated, applied.Report.Rejected})
		require.Equal(t, rec.ID, applied.Report.ID)
	})

	t.Run("an earlier add of the same file is found, except the import itself", func(t *testing.T) {
		reset(t)
		_, found, err := log.LastAppliedAdd(ctx, "h1", "")
		require.NoError(t, err)
		require.False(t, found)

		rec, _, err := log.Create(ctx, applying("", "h1", domain.ImportAdd))
		require.NoError(t, err)
		ok, err := log.Finish(ctx, rec.ID, domain.ImportApplying, app.ImportReport{})
		require.NoError(t, err)
		require.True(t, ok)

		stamp, found, err := log.LastAppliedAdd(ctx, "h1", "")
		require.NoError(t, err)
		require.True(t, found)
		require.False(t, stamp.AppliedAt.IsZero())
		_, found, err = log.LastAppliedAdd(ctx, "h1", rec.ID)
		require.NoError(t, err)
		require.False(t, found)
	})

	t.Run("only one caller holds the imports lock, and nobody waits for it", func(t *testing.T) {
		reset(t)
		_, unlock, locked, err := log.LockImports(ctx)
		require.NoError(t, err)
		require.True(t, locked)
		_, _, second, err := log.LockImports(ctx)
		require.NoError(t, err)
		require.False(t, second, "the second caller is told at once")
		var waiting int
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting))
		require.Zero(t, waiting)
		unlock()
		_, unlockAgain, third, err := log.LockImports(ctx)
		require.NoError(t, err)
		require.True(t, third, "the lock is free once released")
		unlockAgain()
	})

	t.Run("a connection whose lock was lost is not returned to the pool", func(t *testing.T) {
		reset(t)
		locked, unlock, ok, err := log.LockImports(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		var pid int
		require.NoError(t, database.Q(locked, f.pool).QueryRow(locked, `SELECT pg_backend_pid()`).Scan(&pid))
		_, err = database.Q(locked, f.pool).Exec(locked, `SELECT pg_advisory_unlock_all()`)
		require.NoError(t, err)

		unlock()
		require.Eventually(t, func() bool {
			var alive int
			return f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&alive) == nil && alive == 0
		}, 10*time.Second, 10*time.Millisecond, "the connection was closed instead of going back to the pool")
	})

	t.Run("the unlock does not inherit the cancellation of the request", func(t *testing.T) {
		reset(t)
		request, cancel := context.WithCancel(ctx)
		locked, unlock, ok, err := log.LockImports(request)
		require.NoError(t, err)
		require.True(t, ok)
		var pid int
		require.NoError(t, database.Q(locked, f.pool).QueryRow(locked, `SELECT pg_backend_pid()`).Scan(&pid))
		cancel()

		unlock()
		var held, alive int
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'`).Scan(&held))
		require.Zero(t, held, "the lock is released at once although the request was cancelled")
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&alive))
		require.Equal(t, 1, alive, "a clean unlock keeps the connection for the pool")
	})

	t.Run("an applied key is found without the lock", func(t *testing.T) {
		reset(t)
		_, found, err := log.ByKey(ctx, importKey)
		require.NoError(t, err)
		require.False(t, found)
		rec, created, err := log.Create(ctx, app.ImportRecord{Key: importKey, Mode: domain.ImportAdd, Status: domain.ImportApplying, File: app.ImportFile{Name: "a.csv", SHA256: "h1"}})
		require.NoError(t, err)
		require.True(t, created)
		got, found, err := log.ByKey(ctx, importKey)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, rec.ID, got.ID)
	})
}
