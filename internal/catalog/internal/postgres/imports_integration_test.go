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
	"github.com/skolldire/shops/internal/platform/database/dbtest"
)

const importKey = "6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"

func preview(key, hash string) app.ImportRecord {
	return app.ImportRecord{
		Key: key, Mode: domain.ImportAdd, Status: domain.ImportPreview,
		File:    app.ImportFile{Name: "stock.csv", Bytes: 3, SHA256: hash},
		Content: []byte("csv"),
		Preview: []domain.PreviewLine{{SKU: "A-1", Before: 5, After: 7, Reserved: 1}},
	}
}

func TestImportLog(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	log, err := postgres.NewImportLog(f.pool, time.Hour, 2)
	require.NoError(t, err)
	reset := func(t *testing.T) { t.Helper(); dbtest.Truncate(t, f.pool, "imports") }
	age := func(t *testing.T, id string) {
		t.Helper()
		_, err := f.pool.Exec(ctx, `UPDATE imports SET created_at = now() - interval '2 hours' WHERE id = $1::uuid`, id)
		require.NoError(t, err)
	}

	t.Run("a preview keeps its file, content and preview, and a key returns the same import", func(t *testing.T) {
		reset(t)
		rec, created, err := log.CreatePreview(ctx, preview(importKey, "h1"))
		require.NoError(t, err)
		require.True(t, created)
		require.Equal(t, domain.ImportPreview, rec.Status)
		require.Equal(t, []byte("csv"), rec.Content)
		require.Equal(t, []domain.PreviewLine{{SKU: "A-1", Before: 5, After: 7, Reserved: 1}}, rec.Preview)
		require.Equal(t, app.ImportFile{Name: "stock.csv", Bytes: 3, SHA256: "h1"}, rec.File)
		require.False(t, rec.Expired)

		again, created, err := log.CreatePreview(ctx, preview(importKey, "other"))
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, rec.ID, again.ID)
		require.Equal(t, "h1", again.File.SHA256, "the stored import answers, not the new request")
	})

	t.Run("at most the configured previews keep their content, and expired ones are purged first", func(t *testing.T) {
		reset(t)
		first, _, err := log.CreatePreview(ctx, preview("", "h1"))
		require.NoError(t, err)
		_, _, err = log.CreatePreview(ctx, preview("", "h2"))
		require.NoError(t, err)
		_, _, err = log.CreatePreview(ctx, preview("", "h3"))
		require.ErrorIs(t, err, app.ErrTooManyPreviews)

		age(t, first.ID)
		stale, err := log.Get(ctx, first.ID)
		require.NoError(t, err)
		require.True(t, stale.Expired)
		_, created, err := log.CreatePreview(ctx, preview("", "h3"))
		require.NoError(t, err)
		require.True(t, created)
		purged, err := log.Get(ctx, first.ID)
		require.NoError(t, err)
		require.Equal(t, domain.ImportCancelled, purged.Status)
		require.Equal(t, domain.CancelledExpired, purged.CancelReason)
		require.Nil(t, purged.Content)
	})

	t.Run("each transition happens once and only from its status", func(t *testing.T) {
		reset(t)
		rec, _, err := log.CreatePreview(ctx, preview("", "h1"))
		require.NoError(t, err)
		ok, err := log.StartApplying(ctx, rec.ID)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = log.StartApplying(ctx, rec.ID)
		require.NoError(t, err)
		require.False(t, ok)
		ok, err = log.Cancel(ctx, rec.ID, domain.CancelledByUser)
		require.NoError(t, err)
		require.False(t, ok, "an import that is being applied cannot be cancelled")

		report := app.ImportReport{Records: 3, Created: 1, Updated: 1, Rejected: 1, PreviewChanged: true,
			Errors: []app.RowError{{Line: 4, SKU: "B-1", Field: "stock", Code: "below_reserved", Message: "m"}}}
		ok, err = log.Finish(ctx, rec.ID, domain.ImportApplying, report)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = log.Finish(ctx, rec.ID, domain.ImportApplying, report)
		require.NoError(t, err)
		require.False(t, ok)

		applied, err := log.Get(ctx, rec.ID)
		require.NoError(t, err)
		require.Equal(t, domain.ImportApplied, applied.Status)
		require.Nil(t, applied.Content, "the content is dropped once applied")
		require.False(t, applied.Stamp.AppliedAt.IsZero())
		require.Equal(t, report.Errors, applied.Report.Errors)
		require.Equal(t, []int{3, 1, 1, 1}, []int{applied.Report.Records, applied.Report.Created, applied.Report.Updated, applied.Report.Rejected})
		require.True(t, applied.Report.PreviewChanged)
		require.Equal(t, rec.ID, applied.Report.ID)
		require.Equal(t, domain.ImportAdd, applied.Report.Mode)
	})

	t.Run("an expired preview cannot start and only a preview can be cancelled", func(t *testing.T) {
		reset(t)
		rec, _, err := log.CreatePreview(ctx, preview("", "h1"))
		require.NoError(t, err)
		age(t, rec.ID)
		ok, err := log.StartApplying(ctx, rec.ID)
		require.NoError(t, err)
		require.False(t, ok)
		ok, err = log.Cancel(ctx, rec.ID, domain.CancelledByUser)
		require.NoError(t, err)
		require.True(t, ok)
		cancelled, err := log.Get(ctx, rec.ID)
		require.NoError(t, err)
		require.Equal(t, []any{domain.ImportCancelled, domain.CancelledByUser, []byte(nil)}, []any{cancelled.Status, cancelled.CancelReason, cancelled.Content})

		_, err = log.Get(ctx, "9b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b")
		require.ErrorIs(t, err, app.ErrImportNotFound)
	})

	t.Run("an earlier add of the same file is found, except the import itself", func(t *testing.T) {
		reset(t)
		_, found, err := log.LastAppliedAdd(ctx, "h1", "")
		require.NoError(t, err)
		require.False(t, found)

		rec, _, err := log.Create(ctx, app.ImportRecord{Mode: domain.ImportAdd, Status: domain.ImportPreview, File: app.ImportFile{Name: "a.csv", SHA256: "h1"}})
		require.NoError(t, err)
		ok, err := log.Finish(ctx, rec.ID, domain.ImportPreview, app.ImportReport{})
		require.NoError(t, err)
		require.True(t, ok)

		stamp, found, err := log.LastAppliedAdd(ctx, "h1", "")
		require.NoError(t, err)
		require.True(t, found)
		require.False(t, stamp.AppliedAt.IsZero())
		_, found, err = log.LastAppliedAdd(ctx, "h1", rec.ID)
		require.NoError(t, err)
		require.False(t, found)
		_, found, err = log.LastAppliedAdd(ctx, "h2", "")
		require.NoError(t, err)
		require.False(t, found)
	})

	t.Run("a second lock on the same import waits for the first", func(t *testing.T) {
		reset(t)
		_, unlock, err := log.Lock(ctx, importKey)
		require.NoError(t, err)
		second := async(func() (func(), error) {
			_, unlock, err := log.Lock(ctx, importKey)
			return unlock, err
		})
		require.Eventually(t, func() bool {
			var waiting int
			err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting)
			return err == nil && waiting == 1
		}, 10*time.Second, 10*time.Millisecond)
		unlock()
		unlockSecond, err := second()
		require.NoError(t, err)
		unlockSecond()
	})
}
