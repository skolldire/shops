package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/app/apptest"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

type fakeTargets map[string]domain.ImportTarget

func (f fakeTargets) ImportTargets(_ context.Context, skus []string) (map[string]domain.ImportTarget, error) {
	out := map[string]domain.ImportTarget{}
	for _, sku := range skus {
		if t, ok := f[sku]; ok {
			out[sku] = t
		}
	}
	return out, nil
}

type csvRows struct{}

func (csvRows) Rows(content []byte) ([]app.ImportRow, error) {
	return rowsOf(string(content)), nil
}

func newPreviews(t *testing.T, targets fakeTargets) (*app.Previews, *apptest.Log, *fakeStore) {
	t.Helper()
	store, log := &fakeStore{}, apptest.NewLog()
	importer, err := app.NewImporter(store, log, apptest.Tx{}, &fakeRecorder{}, &fakeFailures{})
	require.NoError(t, err)
	previews, err := app.NewPreviews(importer, log, targets, csvRows{})
	require.NoError(t, err)
	return previews, log, store
}

const previewBody = csvHeader +
	"Old,OLD-1,,A,5,3,1\n" +
	"Held,HELD-1,,A,5,1,1\n" +
	"Gone,GONE-1,,A,5,1,1\n" +
	"New,NEW-1,,A,5,4,1\n" +
	"Bad,BAD-1,,A,free,1,1\n"

var previewTargets = fakeTargets{
	"OLD-1":  {Stock: 10, Reserved: 1},
	"HELD-1": {Stock: 5, Reserved: 2},
	"GONE-1": {Stock: 1, Deleted: true},
}

func TestAPreviewShowsNewExistingAndRejectedRowsWithoutApplyingThem(t *testing.T) {
	for mode, want := range map[domain.ImportMode]struct {
		existing, rejected int
		lines              []domain.PreviewLine
	}{
		domain.ImportReplace: {1, 3, []domain.PreviewLine{{SKU: "OLD-1", Before: 10, After: 3, Reserved: 1}, {SKU: "HELD-1", Before: 5, After: 1, Reserved: 2}}},
		domain.ImportAdd:     {2, 2, []domain.PreviewLine{{SKU: "OLD-1", Before: 10, After: 13, Reserved: 1}, {SKU: "HELD-1", Before: 5, After: 6, Reserved: 2}}},
	} {
		t.Run(string(mode), func(t *testing.T) {
			previews, log, store := newPreviews(t, previewTargets)
			id, err := previews.Create(context.Background(), keyed(mode, importKey, "h", previewBody))
			require.NoError(t, err)
			require.Empty(t, store.batches, "a preview applies nothing")

			view, err := previews.Get(context.Background(), id)
			require.NoError(t, err)
			require.Equal(t, domain.ImportPreview, view.Record.Status)
			require.Equal(t, 1, view.Current.New)
			require.Equal(t, want.existing, view.Current.Existing)
			require.Equal(t, want.rejected, view.Current.Report.Rejected)
			require.ElementsMatch(t, want.lines, view.Current.Lines)
			stored, err := log.Get(context.Background(), id)
			require.NoError(t, err)
			require.ElementsMatch(t, want.lines, stored.Preview, "the preview is kept to compare it when confirming")
		})
	}
	previews, _, _ := newPreviews(t, previewTargets)
	id, err := previews.Create(context.Background(), keyed(domain.ImportReplace, importKey, "h", previewBody))
	require.NoError(t, err)
	view, err := previews.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, []string{"3 HELD-1 stock below_reserved", "4 GONE-1 sku sku_deleted", "6 BAD-1 price invalid_format"}, codes(view.Current.Report))
}

func TestTheSameKeyShowsTheSamePreviewAndAnotherFileIsAConflict(t *testing.T) {
	previews, _, _ := newPreviews(t, previewTargets)
	id, err := previews.Create(context.Background(), keyed(domain.ImportAdd, importKey, "h", previewBody))
	require.NoError(t, err)
	again, err := previews.Create(context.Background(), keyed(domain.ImportAdd, importKey, "h", previewBody))
	require.NoError(t, err)
	require.Equal(t, id, again)
	_, err = previews.Create(context.Background(), keyed(domain.ImportAdd, importKey, "other", previewBody))
	require.ErrorIs(t, err, app.ErrIdempotencyConflict)
}

func TestAPreviewWarnsWhenTheSameFileWasAlreadyAdded(t *testing.T) {
	previews, log, _ := newPreviews(t, previewTargets)
	earlier := app.ImportRecord{ID: "00000000-0000-4000-8000-000000000050", Mode: domain.ImportAdd, Status: domain.ImportApplied, File: app.ImportFile{SHA256: "h"}}
	log.Set(earlier)
	id, err := previews.Create(context.Background(), keyed(domain.ImportAdd, importKey, "h", previewBody))
	require.NoError(t, err)
	view, err := previews.Get(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, view.Duplicate)

	replace, err := previews.Create(context.Background(), keyed(domain.ImportReplace, otherKey, "h", previewBody))
	require.NoError(t, err)
	view, err = previews.Get(context.Background(), replace)
	require.NoError(t, err)
	require.Nil(t, view.Duplicate, "only an add of the same file is a duplicate")
}

func TestAnExpiredPreviewIsCancelledWhenItIsRead(t *testing.T) {
	previews, log, _ := newPreviews(t, previewTargets)
	id, err := previews.Create(context.Background(), keyed(domain.ImportAdd, importKey, "h", previewBody))
	require.NoError(t, err)
	stale, err := log.Get(context.Background(), id)
	require.NoError(t, err)
	stale.Expired = true
	log.Set(stale)

	view, err := previews.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, domain.ImportCancelled, view.Record.Status)
	require.Equal(t, domain.CancelledExpired, view.Record.CancelReason)

	_, err = previews.Get(context.Background(), "not-an-id")
	require.ErrorIs(t, err, app.ErrImportNotFound)
}
