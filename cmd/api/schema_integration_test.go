//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/lifecycle"
	"github.com/skolldire/shops/internal/platform/logger"
)

func TestStartRefusesAnOutdatedSchema(t *testing.T) {
	c := startApp(t)
	_, err := c.pool.Exec(context.Background(), `ALTER TABLE inventory_movements DROP COLUMN reserved_after`)
	require.NoError(t, err)
	cfg := c.cfg
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.Metrics.Addr = "127.0.0.1:0"
	cfg.Demo.Seed = false
	var logs syncBuffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	lc, err := lifecycle.New(log)
	require.NoError(t, err)

	err = start(t.Context(), cfg, log, lc)

	require.ErrorIs(t, err, database.ErrSchemaOutdated)
	require.EqualError(t, err, "database: the schema is out of date, recreate the volume with make down && make up")
	require.NoError(t, lc.Close(context.Background()))
	require.NotContains(t, logs.String(), "server listening")
	require.NotContains(t, logs.String(), "reserved_after")
}
