//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/csvimport"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

type noRecorder struct{}

func (noRecorder) RecordRows(context.Context, string, int) {}

type noFailures struct{}

func (noFailures) BatchRetried(context.Context, int, error)        {}
func (noFailures) RowRejected(context.Context, int, string, error) {}

func item(sku, name, price string, stock int) domain.Product {
	return domain.Product{SKU: sku, Name: name, Description: "about " + name, Category: "Imports", Price: d(price), Stock: stock, WeightKg: d("1.5")}
}

type stored struct {
	Name      string
	Stock     int
	Version   int
	Status    string
	UpdatedAt time.Time
}

func (f fixture) stored(t *testing.T, sku string) stored {
	t.Helper()
	var s stored
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT name, stock, version, status, updated_at FROM products WHERE sku = $1`, sku).
		Scan(&s.Name, &s.Stock, &s.Version, &s.Status, &s.UpdatedAt))
	return s
}

func (f fixture) count(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM products`).Scan(&n))
	return n
}

func (f fixture) importer(t *testing.T) *csvimport.Importer {
	t.Helper()
	rows, err := app.NewImporter(f.repo, noRecorder{}, noFailures{})
	require.NoError(t, err)
	importer, err := csvimport.NewImporter(rows, csvimport.Limits{MaxBytes: 10 << 20, MaxRows: 10000})
	require.NoError(t, err)
	return importer
}

func generated(rows int) string {
	var b strings.Builder
	b.WriteString("name,sku,description,category,price,stock,weight_kg\n")
	for i := rows; i > 0; i-- {
		fmt.Fprintf(&b, "Item %d,GEN-%05d,\"Generated, row %d\",Generated,%d.99,%d,0.250\n", i, i, i, i%900+1, i%50)
	}
	return b.String()
}

func TestUpsert(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	t.Run("creates, updates and leaves unchanged rows alone", func(t *testing.T) {
		f.reset(t)
		first, err := f.repo.Upsert(ctx, []domain.Product{item("A-1", "Alpha", "1.00", 1), item("B-1", "Beta", "2.00", 2)})
		require.NoError(t, err)
		require.Equal(t, map[string]app.UpsertOutcome{"A-1": app.OutcomeCreated, "B-1": app.OutcomeCreated}, first)
		before := f.stored(t, "A-1")

		second, err := f.repo.Upsert(ctx, []domain.Product{
			item("A-1", "Alpha", "1.0", 1), item("B-1", "Beta", "2.00", 9), item("C-1", "Gamma", "3.00", 3),
		})

		require.NoError(t, err)
		require.Equal(t, map[string]app.UpsertOutcome{
			"A-1": app.OutcomeUnchanged, "B-1": app.OutcomeUpdated, "C-1": app.OutcomeCreated,
		}, second)
		require.Equal(t, before, f.stored(t, "A-1"), "an unchanged row keeps its version and updated_at")
		changed := f.stored(t, "B-1")
		require.Equal(t, 9, changed.Stock, "stock is absolute")
		require.Equal(t, 2, changed.Version)
		require.Equal(t, 1, f.stored(t, "C-1").Version)
	})

	t.Run("importing the same rows again changes nothing", func(t *testing.T) {
		f.reset(t)
		rows := []domain.Product{item("A-1", "Alpha", "1.00", 1), item("B-1", "Beta", "2.00", 2)}
		_, err := f.repo.Upsert(ctx, rows)
		require.NoError(t, err)
		before := []stored{f.stored(t, "A-1"), f.stored(t, "B-1")}

		again, err := f.repo.Upsert(ctx, rows)

		require.NoError(t, err)
		require.Equal(t, map[string]app.UpsertOutcome{"A-1": app.OutcomeUnchanged, "B-1": app.OutcomeUnchanged}, again)
		require.Equal(t, before, []stored{f.stored(t, "A-1"), f.stored(t, "B-1")})
	})

	t.Run("a deleted sku is reported and never revived", func(t *testing.T) {
		f.reset(t)
		gone := f.create(t, "GONE-1", "Gone", "Imports", "1.00", 1)
		require.NoError(t, f.repo.Delete(ctx, gone.ID, gone.Version))
		before := f.stored(t, "GONE-1")

		outcomes, err := f.repo.Upsert(ctx, []domain.Product{item("GONE-1", "Back again", "5.00", 5), item("NEW-1", "New", "1.00", 1)})

		require.NoError(t, err)
		require.Equal(t, map[string]app.UpsertOutcome{"GONE-1": app.OutcomeDeleted, "NEW-1": app.OutcomeCreated}, outcomes)
		require.Equal(t, before, f.stored(t, "GONE-1"))
		require.Equal(t, "DELETED", before.Status)

		only, err := f.repo.Upsert(ctx, []domain.Product{item("GONE-1", "Back again", "5.00", 5)})
		require.NoError(t, err)
		require.Equal(t, map[string]app.UpsertOutcome{"GONE-1": app.OutcomeDeleted}, only)
	})

	t.Run("a product deleted while the upsert waits for it is reported as deleted", func(t *testing.T) {
		f.reset(t)
		racing := f.create(t, "RACE-1", "Race", "Imports", "1.00", 1)
		deletion, err := f.pool.Begin(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = deletion.Rollback(context.Background()) })
		_, err = deletion.Exec(ctx, `UPDATE products SET status = 'DELETED', deleted_at = now() WHERE id = $1::uuid`, racing.ID)
		require.NoError(t, err)

		type result struct {
			outcomes map[string]app.UpsertOutcome
			err      error
		}
		done := make(chan result, 1)
		go func() {
			outcomes, err := f.repo.Upsert(ctx, []domain.Product{item("RACE-1", "Race again", "9.00", 9), item("RACE-2", "Other", "1.00", 1)})
			done <- result{outcomes, err}
		}()
		require.Eventually(t, func() bool {
			var waiting int
			err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE wait_event_type = 'Lock' AND query LIKE '%INSERT INTO products%' AND pid <> pg_backend_pid()`).Scan(&waiting)
			return err == nil && waiting == 1
		}, 10*time.Second, 20*time.Millisecond, "the upsert never waited for the row being deleted")
		require.NoError(t, deletion.Commit(ctx))

		got := <-done
		require.NoError(t, got.err)
		require.Equal(t, map[string]app.UpsertOutcome{"RACE-1": app.OutcomeDeleted, "RACE-2": app.OutcomeCreated}, got.outcomes)
		after := f.stored(t, "RACE-1")
		require.Equal(t, "DELETED", after.Status)
		require.Equal(t, "Race", after.Name)
	})

	t.Run("an empty batch does nothing", func(t *testing.T) {
		f.reset(t)
		outcomes, err := f.repo.Upsert(ctx, nil)
		require.NoError(t, err)
		require.Empty(t, outcomes)
	})

	t.Run("a batch the database rejects applies none of its rows", func(t *testing.T) {
		f.reset(t)
		_, err := f.repo.Upsert(ctx, []domain.Product{item("OK-1", "Fine", "1.00", 1), item("BAD-1", "Free", "0", 1)})

		require.Error(t, err)
		require.Zero(t, f.count(t))
	})

	t.Run("knows whether the table has any product, deleted or not", func(t *testing.T) {
		f.reset(t)
		has, err := f.repo.HasProducts(ctx)
		require.NoError(t, err)
		require.False(t, has)

		gone := f.create(t, "GONE-1", "Gone", "Imports", "1.00", 1)
		require.NoError(t, f.repo.Delete(ctx, gone.ID, gone.Version))

		has, err = f.repo.HasProducts(ctx)
		require.NoError(t, err)
		require.True(t, has)
	})
}

func TestImportIntoPostgres(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	t.Run("files larger than one batch", func(t *testing.T) {
		f.reset(t)
		body := generated(1201)

		report, err := f.importer(t).Import(ctx, strings.NewReader(body), "generated.csv")

		require.NoError(t, err)
		require.Equal(t, 1201, report.Created)
		require.Empty(t, report.Errors)
		require.Equal(t, 1201, f.count(t))

		again, err := f.importer(t).Import(ctx, strings.NewReader(body), "generated.csv")
		require.NoError(t, err)
		require.Equal(t, 1201, again.Unchanged)
		require.Zero(t, again.Created+again.Updated+again.Rejected)
	})

	t.Run("a row the database rejects does not stop the rest of its batch", func(t *testing.T) {
		f.reset(t)
		_, err := f.pool.Exec(ctx, `ALTER TABLE products ADD CONSTRAINT no_cursed CHECK (name <> 'Cursed')`)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := f.pool.Exec(context.Background(), `ALTER TABLE products DROP CONSTRAINT no_cursed`)
			require.NoError(t, err)
		})
		body := "name,sku,description,category,price,stock,weight_kg\nFine,OK-1,,A,1,1,1\nCursed,BAD-1,,A,1,1,1\nAlso fine,OK-2,,A,1,1,1\n"

		report, err := f.importer(t).Import(ctx, strings.NewReader(body), "products.csv")

		require.NoError(t, err)
		require.Equal(t, 2, report.Created)
		require.Equal(t, 1, report.Rejected)
		require.Equal(t, []app.RowError{{Line: 3, SKU: "BAD-1", Field: "row", Code: app.CodeRejectedByDatabase, Message: "could not be saved"}}, report.Errors)
		require.Equal(t, 2, f.count(t))
	})

	t.Run("two imports of the same file at once do not deadlock", func(t *testing.T) {
		f.reset(t)
		body := generated(1500)
		reports := make([]app.ImportReport, 4)
		errs := make([]error, len(reports))
		importer := f.importer(t)
		var wg sync.WaitGroup
		for i := range reports {
			wg.Go(func() {
				reports[i], errs[i] = importer.Import(ctx, strings.NewReader(body), "generated.csv")
			})
		}
		wg.Wait()

		created := 0
		for i, report := range reports {
			require.NoError(t, errs[i])
			require.Empty(t, report.Errors)
			require.Equal(t, 1500, report.Created+report.Updated+report.Unchanged)
			require.Zero(t, report.Updated)
			created += report.Created
		}
		require.Equal(t, 1500, created, "each row is created by exactly one of the imports")
		require.Equal(t, 1500, f.count(t))
	})

	t.Run("ten thousand valid rows in under ten seconds", func(t *testing.T) {
		f.reset(t)
		body := generated(10000)
		start := time.Now()

		report, err := f.importer(t).Import(ctx, strings.NewReader(body), "generated.csv")

		require.NoError(t, err)
		require.Equal(t, 10000, report.Created)
		require.Less(t, time.Since(start), 10*time.Second)
	})
}
