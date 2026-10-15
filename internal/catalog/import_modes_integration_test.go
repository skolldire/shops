//go:build integration

package catalog_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

const header = "name,sku,description,category,price,stock,weight_kg\n"

type replicas struct {
	pool *pgxpool.Pool
	a, b *catalog.Module
}

func twoReplicas(t *testing.T, maxPreviews int) replicas {
	t.Helper()
	pool, _ := dbtest.Start(t)
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	module := func() *catalog.Module {
		m, err := catalog.NewModule(catalog.Deps{
			Pool: pool, Log: log, Metrics: telemetry.Noop(), Tracer: telemetry.Noop(),
			Import: catalog.ImportLimits{MaxBytes: 1 << 20, MaxRows: 1000, PreviewTTL: time.Hour, MaxActivePreviews: maxPreviews},
		})
		require.NoError(t, err)
		return m
	}
	return replicas{pool: pool, a: module(), b: module()}
}

func (r replicas) stock(t *testing.T, sku string) int {
	t.Helper()
	var n int
	require.NoError(t, r.pool.QueryRow(context.Background(), `SELECT stock FROM products WHERE sku = $1`, sku).Scan(&n))
	return n
}

func (r replicas) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, r.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func add(key string, allow bool) catalog.ImportOptions {
	return catalog.ImportOptions{FileName: "delivery.csv", Mode: catalog.ImportAdd, IdempotencyKey: key, AllowDuplicate: allow}
}

const (
	keyOne   = "11111111-1111-4111-8111-111111111111"
	keyTwo   = "22222222-2222-4222-8222-222222222222"
	keyThree = "33333333-3333-4333-8333-333333333333"
)

func TestAddImportsAreIdempotentAndAskBeforeRepeatingAFile(t *testing.T) {
	r := twoReplicas(t, 20)
	ctx := context.Background()
	_, err := r.a.Import(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,99.00,5,3\n"), catalog.ImportOptions{FileName: "base.csv"})
	require.NoError(t, err)
	delivery := header + "Tent,TN-1,,Camping,109.00,3,3\n"

	var (
		wg      sync.WaitGroup
		reports = make([]catalog.ImportReport, 4)
		errs    = make([]error, 4)
	)
	for i, m := range []*catalog.Module{r.a, r.b, r.a, r.b} {
		wg.Go(func() { reports[i], errs[i] = m.Import(ctx, strings.NewReader(delivery), add(keyOne, false)) })
	}
	wg.Wait()
	for i := range reports {
		require.NoError(t, errs[i])
		require.Equal(t, reports[0], reports[i], "every submission answers the original report")
	}
	require.Equal(t, 8, r.stock(t, "TN-1"), "a double submission adds the units once")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM inventory_movements WHERE reason = 'import_add'`))

	_, err = r.b.Import(ctx, strings.NewReader(delivery), add(keyTwo, false))
	var duplicate *catalog.DuplicateFileError
	require.ErrorAs(t, err, &duplicate)
	require.WithinDuration(t, time.Now(), duplicate.AppliedAt, time.Minute)
	require.Equal(t, 8, r.stock(t, "TN-1"))

	again, err := r.b.Import(ctx, strings.NewReader(delivery), add(keyTwo, true))
	require.NoError(t, err)
	require.NotEqual(t, reports[0].ID, again.ID)
	require.Equal(t, 11, r.stock(t, "TN-1"), "a confirmed repetition adds the units again")

	_, err = r.a.Import(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,109.00,4,3\n"), add(keyOne, false))
	require.ErrorIs(t, err, catalog.ErrIdempotencyConflict)
	_, err = r.a.Import(ctx, strings.NewReader(delivery), catalog.ImportOptions{Mode: catalog.ImportAdd})
	require.ErrorIs(t, err, catalog.ErrIdempotencyKeyRequired)
}

func TestAnAddImportThatFailsHalfwayChangesNothing(t *testing.T) {
	r := twoReplicas(t, 20)
	ctx := context.Background()
	_, err := r.a.Import(ctx, strings.NewReader(header+"A,A-1,,Tests,1.00,5,1\nZ,Z-9,,Tests,1.00,5,1\n"), catalog.ImportOptions{FileName: "base.csv"})
	require.NoError(t, err)
	_, err = r.pool.Exec(ctx, `
		CREATE FUNCTION fail_z9() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.sku = 'Z-9' THEN
				RAISE EXCEPTION 'synthetic failure on the last row';
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER products_fail_z9 BEFORE UPDATE ON products FOR EACH ROW EXECUTE FUNCTION fail_z9();`)
	require.NoError(t, err)
	movements := r.count(t, `SELECT count(*) FROM inventory_movements`)

	var body strings.Builder
	body.WriteString(header)
	for i := 1; i <= 600; i++ {
		fmt.Fprintf(&body, "New,N-%04d,,Tests,1.00,1,1\n", i)
	}
	body.WriteString("A,A-1,,Tests,1.00,2,1\nZ,Z-9,,Tests,1.00,2,1\n")
	_, err = r.a.Import(ctx, strings.NewReader(body.String()), add(keyOne, false))

	require.ErrorContains(t, err, "synthetic failure on the last row")
	require.Equal(t, 5, r.stock(t, "A-1"), "the rows before the failure are rolled back too")
	require.Equal(t, 2, r.count(t, `SELECT count(*) FROM products`))
	require.Equal(t, movements, r.count(t, `SELECT count(*) FROM inventory_movements`))
	require.Zero(t, r.count(t, `SELECT count(*) FROM imports WHERE mode = 'add'`), "no trace of an import that did not happen")
}

func TestAPreviewIsConfirmedFromAnotherReplicaOnce(t *testing.T) {
	r := twoReplicas(t, 20)
	ctx := context.Background()
	_, err := r.a.Import(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,99.00,5,3\n"), catalog.ImportOptions{FileName: "base.csv"})
	require.NoError(t, err)

	id, err := r.a.PreviewImport(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,99.00,9,3\nLamp,LP-1,,Camping,9.00,2,1\n"),
		catalog.ImportOptions{FileName: "count.csv", Mode: catalog.ImportReplace, IdempotencyKey: keyOne})
	require.NoError(t, err)
	preview, err := r.b.ImportPreview(ctx, id)
	require.NoError(t, err)
	require.Equal(t, catalog.ImportPreviewing, preview.Status)
	require.Equal(t, []int{1, 1}, []int{preview.New, preview.Existing})
	require.Equal(t, []catalog.PreviewRow{{SKU: "TN-1", StockBefore: 5, StockAfter: 9}}, preview.Rows)
	require.Equal(t, 5, r.stock(t, "TN-1"), "a preview changes nothing")

	lock, err := r.pool.Acquire(ctx)
	require.NoError(t, err)
	defer lock.Release()
	_, err = lock.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('catalog.import:' || $1, 0))`, id)
	require.NoError(t, err)
	var (
		wg      sync.WaitGroup
		reports = make([]catalog.ImportReport, 2)
		errs    = make([]error, 2)
	)
	for i, m := range []*catalog.Module{r.a, r.b} {
		wg.Go(func() { reports[i], errs[i] = m.ConfirmImport(ctx, id, false) })
	}
	require.Eventually(t, func() bool {
		return r.count(t, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`) == 2
	}, 10*time.Second, 10*time.Millisecond, "both confirmations wait for the import lock")
	_, err = lock.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended('catalog.import:' || $1, 0))`, id)
	require.NoError(t, err)
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, reports[0], reports[1])
	require.Equal(t, []int{1, 1}, []int{reports[0].Created, reports[0].Updated})
	require.Equal(t, 9, r.stock(t, "TN-1"))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM inventory_movements WHERE reference = $1 AND delta = 4`, id), "applied once")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM imports WHERE id = $1::uuid AND status = 'APPLIED' AND content IS NULL`, id))
	require.ErrorIs(t, r.a.CancelImport(ctx, id), catalog.ErrImportNotCancellable)
}

func TestAReplaceLeftApplyingIsResumedFromItsStoredContent(t *testing.T) {
	r := twoReplicas(t, 20)
	ctx := context.Background()
	id, err := r.a.PreviewImport(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,99.00,5,3\n"),
		catalog.ImportOptions{FileName: "count.csv", Mode: catalog.ImportReplace, IdempotencyKey: keyOne})
	require.NoError(t, err)
	_, err = r.pool.Exec(ctx, `UPDATE imports SET status = 'APPLYING' WHERE id = $1::uuid`, id)
	require.NoError(t, err)
	require.ErrorIs(t, r.a.CancelImport(ctx, id), catalog.ErrImportNotCancellable, "the batches already applied cannot be undone")

	report, err := r.b.ConfirmImport(ctx, id, false)
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)
	require.Equal(t, 5, r.stock(t, "TN-1"))

	again, err := r.a.Import(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,99.00,5,3\n"),
		catalog.ImportOptions{FileName: "count.csv", Mode: catalog.ImportReplace, IdempotencyKey: keyOne})
	require.NoError(t, err)
	require.Equal(t, report, again, "a retry with the same key answers the stored report")
}

func TestPreviewsExpireAndAreLimited(t *testing.T) {
	r := twoReplicas(t, 2)
	ctx := context.Background()
	upload := func(key string) (string, error) {
		return r.a.PreviewImport(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,99.00,5,3\n"),
			catalog.ImportOptions{FileName: "count.csv", Mode: catalog.ImportReplace, IdempotencyKey: key})
	}
	first, err := upload(keyOne)
	require.NoError(t, err)
	_, err = upload(keyTwo)
	require.NoError(t, err)
	_, err = upload(keyThree)
	require.ErrorIs(t, err, catalog.ErrTooManyPreviews)
	same, err := upload(keyOne)
	require.NoError(t, err)
	require.Equal(t, first, same, "repeating an upload does not count against the limit")

	_, err = r.pool.Exec(ctx, `UPDATE imports SET created_at = now() - interval '2 hours' WHERE id = $1::uuid`, first)
	require.NoError(t, err)
	_, err = r.b.ConfirmImport(ctx, first, false)
	require.ErrorIs(t, err, catalog.ErrPreviewExpired)
	expired, err := r.b.ImportPreview(ctx, first)
	require.NoError(t, err)
	require.Equal(t, []any{catalog.ImportCancelled, "expired"}, []any{expired.Status, expired.CancelReason})
	require.Zero(t, r.count(t, `SELECT count(*) FROM imports WHERE id = $1::uuid AND content IS NOT NULL`, first), "its content is freed")

	_, err = upload(keyThree)
	require.NoError(t, err, "the expired preview no longer counts")
	require.Zero(t, r.count(t, `SELECT count(*) FROM products`))
}
