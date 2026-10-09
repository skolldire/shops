//go:build integration

package dbtest

import (
	"context"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/secret"
)

const password = "integration-password"

func Start(t *testing.T) (*pgxpool.Pool, database.Config) {
	t.Helper()
	ctx := context.Background()

	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	schema := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "db", "init", "001_schema.sql")

	ctr, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("shop"),
		tcpostgres.WithUsername("shop"),
		tcpostgres.WithPassword(password),
		tcpostgres.WithInitScripts(schema),
		tcpostgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	require.NoError(t, err)

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	portNum, err := strconv.Atoi(port.Port())
	require.NoError(t, err)

	cfg := database.Config{
		Host: host, Port: portNum, Name: "shop", User: "shop",
		Password: secret.New(password), SSLMode: "disable",
		MaxConns: 4, ConnectTimeout: 10 * time.Second,
	}
	pool, err := database.Open(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, cfg
}
