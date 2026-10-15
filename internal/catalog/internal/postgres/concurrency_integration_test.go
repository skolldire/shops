//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/database"
)

var errRollback = errors.New("the test rolls back")

func (f fixture) hold(t *testing.T, fn func(ctx context.Context) error) func(commit bool) error {
	t.Helper()
	tm, err := database.NewTxManager(f.pool)
	require.NoError(t, err)
	held, proceed, done := make(chan error, 1), make(chan bool), make(chan error, 1)
	go func() {
		done <- tm.WithinTx(context.Background(), func(ctx context.Context) error {
			if err := fn(ctx); err != nil {
				held <- err
				return err
			}
			held <- nil
			if !<-proceed {
				return errRollback
			}
			return nil
		})
	}()
	require.NoError(t, <-held)
	return func(commit bool) error {
		proceed <- commit
		if err := <-done; !errors.Is(err, errRollback) {
			return err
		}
		return nil
	}
}

func (f fixture) waitUntilBlocked(t *testing.T, statement string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int
		err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%' AND pid <> pg_backend_pid()`, statement).Scan(&waiting)
		return err == nil && waiting == 1
	}, 10*time.Second, 10*time.Millisecond, "%q never waited for the lock", statement)
}

func async[T any](fn func() (T, error)) func() (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		done <- result{v, err}
	}()
	return func() (T, error) {
		r := <-done
		return r.value, r.err
	}
}

const (
	importStatement  = "INSERT INTO products"
	reserveStatement = "SET reserved = reserved +"
	releaseStatement = "SET reserved = reserved -"
	commitStatement  = "SET stock = stock - $2, reserved = reserved - $2"
)

func TestImportsAndReservationsSerialize(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	req := func(p domain.Product, q int) []domain.StockRequest {
		return []domain.StockRequest{{ProductID: p.ID, Quantity: q}}
	}
	importStock := func(stock int) func() (map[string]app.UpsertOutcome, error) {
		return async(func() (map[string]app.UpsertOutcome, error) {
			return f.repo.Upsert(ctx, []domain.Product{item("A-1", "Alpha", "1.00", stock)}, "sha")
		})
	}
	reserved := func(t *testing.T, a domain.Product, q int) {
		t.Helper()
		_, err := f.repo.ReserveStock(ctx, req(a, q), "purchase-0")
		require.NoError(t, err)
	}

	t.Run("an import waits for a reservation and keeps it", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Imports", "1.00", 5)
		finish := f.hold(t, func(ctx context.Context) error {
			_, err := f.repo.ReserveStock(ctx, req(a, 3), "purchase-1")
			return err
		})
		imported := importStock(10)
		f.waitUntilBlocked(t, importStatement)
		require.NoError(t, finish(true))
		outcomes, err := imported()
		require.NoError(t, err)
		require.Equal(t, app.OutcomeUpdated, outcomes["A-1"])
		require.Equal(t, stockState{10, 3, 2}, f.state(t, a.ID))
		f.requireBalanced(t)
	})

	t.Run("an import that waited for a release sees the released units", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Imports", "1.00", 5)
		reserved(t, a, 3)
		finish := f.hold(t, func(ctx context.Context) error { return f.repo.ReleaseStock(ctx, req(a, 3), "purchase-0") })
		imported := importStock(2)
		f.waitUntilBlocked(t, importStatement)
		require.NoError(t, finish(true))
		outcomes, err := imported()
		require.NoError(t, err)
		require.Equal(t, app.OutcomeUpdated, outcomes["A-1"], "the check runs on the row the release left")
		require.Equal(t, stockState{2, 0, 2}, f.state(t, a.ID))
		f.requireBalanced(t)
	})

	t.Run("an import that waited for a rolled back release is below the reserved units", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Imports", "1.00", 5)
		reserved(t, a, 3)
		finish := f.hold(t, func(ctx context.Context) error { return f.repo.ReleaseStock(ctx, req(a, 3), "purchase-0") })
		imported := importStock(2)
		f.waitUntilBlocked(t, importStatement)
		require.NoError(t, finish(false))
		outcomes, err := imported()
		require.NoError(t, err)
		require.Equal(t, app.OutcomeBelowReserved, outcomes["A-1"])
		require.Equal(t, stockState{5, 3, 1}, f.state(t, a.ID))
	})

	t.Run("an import that waited for a sale replaces the stock left by it", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Imports", "1.00", 5)
		reserved(t, a, 3)
		finish := f.hold(t, func(ctx context.Context) error { return f.repo.CommitStock(ctx, req(a, 3), "purchase-0") })
		imported := importStock(10)
		f.waitUntilBlocked(t, importStatement)
		require.NoError(t, finish(true))
		outcomes, err := imported()
		require.NoError(t, err)
		require.Equal(t, app.OutcomeUpdated, outcomes["A-1"])
		require.Equal(t, stockState{10, 0, 3}, f.state(t, a.ID))
		f.requireBalanced(t)
	})

	t.Run("a reservation waits for an import and counts only what the file left", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Imports", "1.00", 5)
		finish := f.hold(t, func(ctx context.Context) error {
			_, err := f.repo.Upsert(ctx, []domain.Product{item("A-1", "Alpha", "1.00", 1)}, "sha")
			return err
		})
		reservation := async(func() ([]domain.StockLine, error) { return f.repo.ReserveStock(ctx, req(a, 2), "purchase-1") })
		f.waitUntilBlocked(t, reserveStatement)
		require.NoError(t, finish(true))
		_, err := reservation()
		var short *app.InsufficientStockError
		require.ErrorAs(t, err, &short)
		require.Equal(t, 1, short.Available)
		require.Equal(t, stockState{1, 0, 2}, f.state(t, a.ID))
	})

	t.Run("a release waits for an import that was below the reserved units", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Imports", "1.00", 5)
		reserved(t, a, 3)
		var outcomes map[string]app.UpsertOutcome
		finish := f.hold(t, func(ctx context.Context) error {
			var err error
			outcomes, err = f.repo.Upsert(ctx, []domain.Product{item("A-1", "Alpha", "1.00", 2)}, "sha")
			return err
		})
		release := async(func() (struct{}, error) { return struct{}{}, f.repo.ReleaseStock(ctx, req(a, 3), "purchase-0") })
		f.waitUntilBlocked(t, releaseStatement)
		require.NoError(t, finish(true))
		_, err := release()
		require.NoError(t, err)
		require.Equal(t, app.OutcomeBelowReserved, outcomes["A-1"], "the skipped row stayed locked, so the report is exact")
		require.Equal(t, stockState{5, 0, 1}, f.state(t, a.ID))
		f.requireBalanced(t)
	})

	t.Run("a sale waits for an import and settles on the imported stock", func(t *testing.T) {
		f.reset(t)
		a := f.create(t, "A-1", "Alpha", "Imports", "1.00", 5)
		reserved(t, a, 3)
		finish := f.hold(t, func(ctx context.Context) error {
			_, err := f.repo.Upsert(ctx, []domain.Product{item("A-1", "Alpha", "1.00", 10)}, "sha")
			return err
		})
		sale := async(func() (struct{}, error) { return struct{}{}, f.repo.CommitStock(ctx, req(a, 3), "purchase-0") })
		f.waitUntilBlocked(t, commitStatement)
		require.NoError(t, finish(true))
		_, err := sale()
		require.NoError(t, err)
		require.Equal(t, stockState{7, 0, 3}, f.state(t, a.ID))
		f.requireBalanced(t)
	})
}
