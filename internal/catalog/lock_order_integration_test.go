//go:build integration

package catalog_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (r replicas) logged(t *testing.T) (*catalog.Module, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	log, err := logger.New(logger.Config{Level: "info"}, logs)
	require.NoError(t, err)
	m, err := catalog.NewModule(catalog.Deps{
		Pool: r.pool, Log: log, Metrics: telemetry.Noop(), Tracer: telemetry.Noop(),
		Import: catalog.ImportLimits{MaxBytes: 1 << 20, MaxRows: 1000},
	})
	require.NoError(t, err)
	return m, logs
}

const (
	lowID  = "00000000-0000-4000-8000-000000000001"
	highID = "00000000-0000-4000-8000-000000000002"
)

func (r replicas) crossedProducts(t *testing.T) {
	t.Helper()
	_, err := r.pool.Exec(context.Background(), `
		INSERT INTO products (id, sku, name, category, price, stock, weight_kg)
		VALUES ($1::uuid, 'CROSS-B', 'Low id, last sku', 'Tests', 10.00, 5, 1),
		       ($2::uuid, 'CROSS-A', 'High id, first sku', 'Tests', 10.00, 5, 1)`, lowID, highID)
	require.NoError(t, err)
}

func deadlocked(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

type checkout struct {
	reserved chan struct{}
	proceed  chan struct{}
	done     chan error
}

func (r replicas) startCheckout(t *testing.T, m *catalog.Module) checkout {
	t.Helper()
	tm, err := database.NewTxManager(r.pool)
	require.NoError(t, err)
	c := checkout{reserved: make(chan struct{}), proceed: make(chan struct{}), done: make(chan error, 1)}
	go func() {
		c.done <- tm.WithinTx(context.Background(), func(ctx context.Context) error {
			if _, err := m.ReserveStock(ctx, []catalog.StockRequest{{ProductID: lowID, Quantity: 1}}, "purchase-1"); err != nil {
				return err
			}
			close(c.reserved)
			<-c.proceed
			_, err := m.ReserveStock(ctx, []catalog.StockRequest{{ProductID: highID, Quantity: 1}}, "purchase-1")
			return err
		})
	}()
	<-c.reserved
	return c
}

const waitingOnProducts = `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%products%'`

func TestLockingTheProductsBySKUDeadlocksWithACheckout(t *testing.T) {
	r := twoReplicas(t)
	r.crossedProducts(t)
	c := r.startCheckout(t, r.a)

	tm, err := database.NewTxManager(r.pool)
	require.NoError(t, err)
	bySKU := make(chan error, 1)
	go func() {
		bySKU <- tm.WithinTx(context.Background(), func(ctx context.Context) error {
			_, err := database.Q(ctx, r.pool).Exec(ctx, `SELECT 1 FROM products WHERE sku = ANY($1) ORDER BY sku FOR UPDATE`, []string{"CROSS-A", "CROSS-B"})
			return err
		})
	}()
	r.waitFor(t, waitingOnProducts, 1)
	close(c.proceed)

	errs := []error{<-bySKU, <-c.done}
	require.True(t, deadlocked(errs[0]) || deadlocked(errs[1]), "the order of the old import against the checkout is a cycle: %v", errs)
}

func TestACrossedImportAndCheckoutWaitInsteadOfDeadlocking(t *testing.T) {
	for name, opts := range map[string]catalog.ImportOptions{
		"replace": replace(keyOne),
		"add":     add(keyOne, false),
	} {
		t.Run(name, func(t *testing.T) {
			r := twoReplicas(t)
			r.crossedProducts(t)
			c := r.startCheckout(t, r.a)
			importer, logs := r.logged(t)

			imported := make(chan error, 1)
			go func() {
				_, err := importer.Import(context.Background(), strings.NewReader(header+"Low,CROSS-B,,Tests,12.00,10,1\nHigh,CROSS-A,,Tests,12.00,10,1\n"), opts)
				imported <- err
			}()
			r.waitFor(t, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%ORDER BY id FOR UPDATE%'`, 1)
			close(c.proceed)

			require.NoError(t, <-c.done, "the checkout takes its second product: the import holds none while it waits")
			require.NoError(t, <-imported)
			require.NotContains(t, logs.String(), "deadlock", "no batch was retried after a deadlock")
			want := map[string]int{"replace": 10, "add": 15}[name]
			require.Equal(t, want, r.stock(t, "CROSS-A"))
			require.Equal(t, want, r.stock(t, "CROSS-B"))
			require.Equal(t, 2, r.count(t, `SELECT sum(reserved)::int FROM products`))
		})
	}
}
