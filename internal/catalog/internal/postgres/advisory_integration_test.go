//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/database/dbtest"
)

const testLockKey = `hashtextextended('catalog.release-test', 0)`

func TestAFailedUnlockClosesTheConnectionAndFreesTheLock(t *testing.T) {
	pool, _ := dbtest.Start(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		unlock  string
		atLeast time.Duration
	}{
		"the unlock fails with an SQL error":   {`SELECT 1/0 = 1`, 0},
		"the unlock outlives its own deadline": {`SELECT pg_sleep(60) IS NULL`, advisoryReleaseTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := pool.Acquire(ctx)
			require.NoError(t, err)
			_, err = conn.Exec(ctx, `SELECT pg_advisory_lock(`+testLockKey+`)`)
			require.NoError(t, err)
			var pid int
			require.NoError(t, conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
			request, cancel := context.WithCancel(ctx)
			cancel()

			started := time.Now()
			releaseAdvisory(request, conn, tc.unlock)
			elapsed := time.Since(started)

			require.GreaterOrEqual(t, elapsed, tc.atLeast, "the unlock ignores the cancelled request and waits for its own deadline")
			require.Less(t, elapsed, advisoryReleaseTimeout+5*time.Second, "the deadline bounds the unlock")
			require.Eventually(t, func() bool {
				var alive int
				return pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&alive) == nil && alive == 0
			}, 10*time.Second, 10*time.Millisecond, "the physical connection is closed, not returned to the pool")
			require.Eventually(t, func() bool {
				var held int
				return pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'`).Scan(&held) == nil && held == 0
			}, 10*time.Second, 10*time.Millisecond, "no connection keeps the lock")

			other, err := pool.Acquire(ctx)
			require.NoError(t, err)
			defer other.Release()
			var locked bool
			require.NoError(t, other.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+testLockKey+`)`).Scan(&locked))
			require.True(t, locked, "another connection takes the lock afterwards")
			_, err = other.Exec(ctx, `SELECT pg_advisory_unlock(`+testLockKey+`)`)
			require.NoError(t, err)
		})
	}
}
