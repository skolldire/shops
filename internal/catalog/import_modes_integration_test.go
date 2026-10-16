//go:build integration

package catalog_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

func twoReplicas(t *testing.T) replicas {
	t.Helper()
	pool, _ := dbtest.Start(t)
	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	module := func() *catalog.Module {
		m, err := catalog.NewModule(catalog.Deps{
			Pool: pool, Log: log, Metrics: telemetry.Noop(), Tracer: telemetry.Noop(),
			Import: catalog.ImportLimits{MaxBytes: 1 << 20, MaxRows: 1000},
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
	keyOne = "11111111-1111-4111-8111-111111111111"
	keyTwo = "22222222-2222-4222-8222-222222222222"
)

func TestAddImportsAreIdempotentAndAskBeforeRepeatingAFile(t *testing.T) {
	r := twoReplicas(t)
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
	original, err := r.a.Import(ctx, strings.NewReader(delivery), add(keyOne, false))
	require.NoError(t, err, "once the imports are done, the key answers its report")
	for i := range reports {
		if errors.Is(errs[i], catalog.ErrImportInProgress) {
			continue
		}
		require.NoError(t, errs[i])
		require.Equal(t, original, reports[i], "every submission that was not refused answers the original report")
	}
	require.Equal(t, 8, r.stock(t, "TN-1"), "a double submission adds the units once")
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM inventory_movements WHERE reason = 'import_add'`))

	_, err = r.b.Import(ctx, strings.NewReader(delivery), add(keyTwo, false))
	var duplicate *catalog.DuplicateFileError
	require.ErrorAs(t, err, &duplicate)
	require.WithinDuration(t, time.Now(), duplicate.AppliedAt, time.Minute)
	require.Equal(t, 8, r.stock(t, "TN-1"))

	again, err := r.b.Import(ctx, strings.NewReader(delivery), add(keyTwo, true))
	require.NoError(t, err, "the same key works again, because the refused import left no row")
	require.NotEqual(t, reports[0].ID, again.ID)
	require.Equal(t, 11, r.stock(t, "TN-1"), "a confirmed repetition adds the units again")

	_, err = r.a.Import(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,109.00,4,3\n"), add(keyOne, false))
	require.ErrorIs(t, err, catalog.ErrIdempotencyConflict)
	_, err = r.a.Import(ctx, strings.NewReader(delivery), catalog.ImportOptions{Mode: catalog.ImportAdd})
	require.ErrorIs(t, err, catalog.ErrIdempotencyKeyRequired)
}

func TestAnAddImportThatFailsHalfwayChangesNothing(t *testing.T) {
	r := twoReplicas(t)
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

func replaceFile(rows int) string {
	var body strings.Builder
	body.WriteString(header)
	for i := 1; i <= rows; i++ {
		fmt.Fprintf(&body, "Item,N-%04d,,Tests,1.00,%d,1\n", i, i)
	}
	return body.String()
}

func replace(key string) catalog.ImportOptions {
	return catalog.ImportOptions{FileName: "count.csv", Mode: catalog.ImportReplace, IdempotencyKey: key}
}

func (r replicas) holdProduct(t *testing.T, sku string) func() {
	t.Helper()
	tx, err := r.pool.Begin(context.Background())
	require.NoError(t, err)
	_, err = tx.Exec(context.Background(), `SELECT 1 FROM products WHERE sku = $1 FOR UPDATE`, sku)
	require.NoError(t, err)
	return func() { require.NoError(t, tx.Rollback(context.Background())) }
}

func (r replicas) waitFor(t *testing.T, query string, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		return r.pool.QueryRow(context.Background(), query).Scan(&n) == nil && n == want
	}, 10*time.Second, 10*time.Millisecond, query)
}

const blockedImport = `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%ORDER BY id FOR UPDATE%'`

func TestAnInterruptedReplaceIsResumedByUploadingTheFileAgain(t *testing.T) {
	r := twoReplicas(t)
	ctx := context.Background()
	_, err := r.a.Import(ctx, strings.NewReader(header+"Item,N-0550,,Tests,1.00,1,1\n"), catalog.ImportOptions{FileName: "base.csv"})
	require.NoError(t, err)
	file := replaceFile(600)

	release := r.holdProduct(t, "N-0550")
	interrupted, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := r.a.Import(interrupted, strings.NewReader(file), replace(keyOne))
		done <- err
	}()
	r.waitFor(t, blockedImport, 1)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	release()

	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM imports WHERE status = 'APPLYING'`))
	require.Equal(t, 1, r.stock(t, "N-0001"), "the first batch was applied")
	require.Equal(t, 1, r.stock(t, "N-0550"), "the second batch was not")

	report, err := r.b.Import(ctx, strings.NewReader(file), replace(keyOne))
	require.NoError(t, err)
	require.Equal(t, []int{599, 1, 0}, []int{report.Created + report.Unchanged, report.Updated, report.Rejected})
	require.Equal(t, 550, r.stock(t, "N-0550"))
	require.Equal(t, 600, r.stock(t, "N-0600"))
	require.Zero(t, r.count(t, `SELECT count(*) FROM imports WHERE status = 'APPLYING'`), "the resumed import finished")
	require.Zero(t, r.count(t, `
		SELECT count(*) FROM (SELECT product_id FROM inventory_movements WHERE reason = 'import_replace' AND reference = $1
		                      GROUP BY product_id HAVING count(*) > 1) duplicated`, report.ID), "no product moved twice")
	require.Equal(t, 600, r.count(t, `SELECT count(*) FROM inventory_movements WHERE reference = $1`, report.ID))
}

func TestTwoReplicasApplyTheSameReplaceKeyOnce(t *testing.T) {
	r := twoReplicas(t)
	ctx := context.Background()
	_, err := r.a.Import(ctx, strings.NewReader(header+"Item,N-0002,,Tests,1.00,9,1\n"), catalog.ImportOptions{FileName: "base.csv"})
	require.NoError(t, err)
	file := replaceFile(3)

	release := r.holdProduct(t, "N-0002")
	type result struct {
		report catalog.ImportReport
		err    error
	}
	first := make(chan result, 1)
	go func() {
		report, err := r.a.Import(ctx, strings.NewReader(file), replace(keyOne))
		first <- result{report, err}
	}()
	r.waitFor(t, blockedImport, 1)
	_, err = r.b.Import(ctx, strings.NewReader(file), replace(keyOne))
	require.ErrorIs(t, err, catalog.ErrImportInProgress, "the second replica answers at once instead of waiting")
	release()

	a := <-first
	require.NoError(t, a.err)
	b, err := r.b.Import(ctx, strings.NewReader(file), replace(keyOne))
	require.NoError(t, err)
	require.Equal(t, a.report, b, "the retry answers the report of the first")
	require.Equal(t, []int{2, 1}, []int{a.report.Created, a.report.Updated})
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM imports WHERE idempotency_key = $1::uuid`, keyOne))
	require.Equal(t, 3, r.count(t, `SELECT count(*) FROM inventory_movements WHERE reference = $1`, a.report.ID), "applied once")
}

func TestTheReportShowsStockAndPriceBeforeAndAfter(t *testing.T) {
	r := twoReplicas(t)
	ctx := context.Background()
	_, err := r.a.Import(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,29.99,50,3\nLamp,LP-1,,Camping,9.00,2,1\n"), catalog.ImportOptions{FileName: "base.csv"})
	require.NoError(t, err)

	report, err := r.b.Import(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,39.99,10,3\nLamp,LP-1,,Camping,9.00,0,1\nNew,NW-1,,Camping,1.00,1,1\n"), add(keyOne, false))

	require.NoError(t, err)
	require.Len(t, report.Changes, 2, "only existing products")
	tent, lamp := report.Changes[1], report.Changes[0]
	require.Equal(t, []any{"TN-1", 50, 60, "29.99", "39.99"}, []any{tent.SKU, tent.StockBefore, tent.StockAfter, tent.PriceBefore.StringFixed(2), tent.PriceAfter.StringFixed(2)})
	require.Equal(t, []any{"LP-1", 2, 2, "9.00", "9.00"}, []any{lamp.SKU, lamp.StockBefore, lamp.StockAfter, lamp.PriceBefore.StringFixed(2), lamp.PriceAfter.StringFixed(2)})

	again, err := r.a.Import(ctx, strings.NewReader(header+"Tent,TN-1,,Camping,39.99,10,3\nLamp,LP-1,,Camping,9.00,0,1\nNew,NW-1,,Camping,1.00,1,1\n"), add(keyOne, false))
	require.NoError(t, err)
	require.Equal(t, report.Changes[0].SKU, again.Changes[0].SKU)
	require.True(t, report.Changes[1].PriceBefore.Equal(again.Changes[1].PriceBefore), "a repetition answers the stored report, changes included")
}

func TestReplicasStartingTogetherSeedOnce(t *testing.T) {
	r := twoReplicas(t)
	ctx := context.Background()
	official, err := os.ReadFile("../../testdata/products.csv")
	require.NoError(t, err)

	holder, err := r.pool.Acquire(ctx)
	require.NoError(t, err)
	defer holder.Release()
	_, err = holder.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('catalog.seed', 0))`)
	require.NoError(t, err)
	type result struct {
		seeded bool
		err    error
	}
	results := make(chan result, 2)
	for _, m := range []*catalog.Module{r.a, r.b} {
		go func() {
			_, seeded, err := m.Seed(ctx, bytes.NewReader(official))
			results <- result{seeded, err}
		}()
	}
	r.waitFor(t, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`, 2)
	_, err = holder.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended('catalog.seed', 0))`)
	require.NoError(t, err)

	first, second := <-results, <-results
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.NotEqual(t, first.seeded, second.seeded, "one replica seeds and the other finds the products")
	require.Equal(t, 82, r.count(t, `SELECT count(*) FROM products`))
	require.Equal(t, 1, r.count(t, `SELECT count(*) FROM imports WHERE mode = 'replace'`))
}

func TestAnImportWhileAnotherIsApplyingIsRefusedWithoutEffectsAndWorksOnRetry(t *testing.T) {
	r := twoReplicas(t)
	ctx := context.Background()
	_, err := r.a.Import(ctx, strings.NewReader(header+"Item,N-0002,,Tests,1.00,9,1\n"), catalog.ImportOptions{FileName: "base.csv"})
	require.NoError(t, err)

	release := r.holdProduct(t, "N-0002")
	first := make(chan error, 1)
	go func() {
		_, err := r.a.Import(ctx, strings.NewReader(replaceFile(3)), replace(keyOne))
		first <- err
	}()
	r.waitFor(t, blockedImport, 1)

	delivery := header + "Other,O-1,,Tests,2.00,4,1\n"
	_, err = r.b.Import(ctx, strings.NewReader(delivery), add(keyTwo, false))
	require.ErrorIs(t, err, catalog.ErrImportInProgress)
	require.Zero(t, r.count(t, `SELECT count(*) FROM imports WHERE idempotency_key = $1::uuid`, keyTwo), "no import row")
	require.Zero(t, r.count(t, `SELECT count(*) FROM products WHERE sku = 'O-1'`), "no product")
	require.Zero(t, r.count(t, `SELECT count(*) FROM inventory_movements WHERE reason = 'import_add'`), "no movement")

	release()
	require.NoError(t, <-first)
	report, err := r.b.Import(ctx, strings.NewReader(delivery), add(keyTwo, false))
	require.NoError(t, err, "the same request works once the other import is done")
	require.Equal(t, 1, report.Created)
	require.Equal(t, 4, r.stock(t, "O-1"))
}
