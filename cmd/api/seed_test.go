package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/logger"
)

type fakeSeeder struct {
	report catalog.ImportReport
	seeded bool
	err    error
	path   string
}

func (f *fakeSeeder) Seed(_ context.Context, path string) (catalog.ImportReport, bool, error) {
	f.path = path
	return f.report, f.seeded, f.err
}

func runSeed(t *testing.T, s *fakeSeeder) (string, error) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	err = seedDemo(t.Context(), log, s, "/etc/shop/seed/products.csv")
	require.Equal(t, "/etc/shop/seed/products.csv", s.path)
	return logs.String(), err
}

func TestSeedDemoLogsTheSummary(t *testing.T) {
	logs, err := runSeed(t, &fakeSeeder{seeded: true, report: catalog.ImportReport{
		File: catalog.ImportFile{SHA256: "6fc46b84"}, Records: 97, Created: 82, Rejected: 13, Skipped: 2,
	}})

	require.NoError(t, err)
	require.Contains(t, logs, `"message":"demo seed imported"`)
	require.Contains(t, logs, `"severity":"INFO"`)
	require.Contains(t, logs, `"created":82`)
	require.Contains(t, logs, `"rejected":13`)
	require.Contains(t, logs, `"skipped":2`)
	require.NotContains(t, logs, "/etc/shop/seed", "the configured path is never logged")
}

func TestSeedDemoSaysWhenItDoesNothing(t *testing.T) {
	logs, err := runSeed(t, &fakeSeeder{})

	require.NoError(t, err)
	require.Contains(t, logs, "demo seed skipped, the catalog already has products")
	require.NotContains(t, logs, "/etc/shop/seed", "the configured path is never logged")
	require.NotContains(t, logs, "demo seed imported")
}

func TestSeedDemoFailsWhenTheSeedFails(t *testing.T) {
	boom := errors.New("catalog: open seed file: file does not exist")

	logs, err := runSeed(t, &fakeSeeder{err: boom})

	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "demo seed: catalog: open seed file")
	require.Empty(t, logs)
}
