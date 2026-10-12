package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/platform/logger"
)

const seedContent = "name,sku,description,category,price,stock,weight_kg\n"

type fakeSeeder struct {
	report  catalog.ImportReport
	seeded  bool
	err     error
	calls   int
	content string
}

func (f *fakeSeeder) Seed(_ context.Context, file io.Reader) (catalog.ImportReport, bool, error) {
	f.calls++
	content, err := io.ReadAll(file)
	if err != nil {
		return catalog.ImportReport{}, false, err
	}
	f.content = string(content)
	return f.report, f.seeded, f.err
}

func seedFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "demo-products.csv")
	require.NoError(t, os.WriteFile(path, []byte(seedContent), 0o600))
	return path
}

func runSeed(t *testing.T, s *fakeSeeder, path string) (string, error) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	err = seedDemo(t.Context(), log, s, path)
	return logs.String(), err
}

func TestSeedDemoHandsTheFileOverAndLogsTheSummary(t *testing.T) {
	path := seedFile(t)
	s := &fakeSeeder{seeded: true, report: catalog.ImportReport{
		File: catalog.ImportFile{SHA256: "6fc46b84"}, Records: 97, Created: 82, Rejected: 13, Skipped: 2,
	}}

	logs, err := runSeed(t, s, path)

	require.NoError(t, err)
	require.Equal(t, seedContent, s.content)
	require.Contains(t, logs, `"message":"demo seed imported"`)
	require.Contains(t, logs, `"severity":"INFO"`)
	require.Contains(t, logs, `"created":82`)
	require.Contains(t, logs, `"rejected":13`)
	require.Contains(t, logs, `"skipped":2`)
	require.NotContains(t, logs, "demo-products", "the configured path is never logged")
}

func TestSeedDemoSaysWhenItDoesNothing(t *testing.T) {
	logs, err := runSeed(t, &fakeSeeder{}, seedFile(t))

	require.NoError(t, err)
	require.Contains(t, logs, "demo seed skipped, the catalog already has products")
	require.NotContains(t, logs, "demo seed imported")
	require.NotContains(t, logs, "demo-products", "the configured path is never logged")
}

func TestSeedDemoLogsWhatWasAppliedWhenTheSeedIsInterrupted(t *testing.T) {
	logs, err := runSeed(t, &fakeSeeder{err: context.Canceled, report: catalog.ImportReport{
		File: catalog.ImportFile{SHA256: "6fc46b84"}, Records: 1200, Created: 500, Rejected: 3,
	}}, seedFile(t))

	require.ErrorIs(t, err, context.Canceled)
	require.Contains(t, logs, `"message":"demo seed interrupted, part of the file was applied"`)
	require.Contains(t, logs, `"severity":"WARN"`)
	require.Contains(t, logs, `"records":1200`)
	require.Contains(t, logs, `"created":500`)
	require.Contains(t, logs, `"rejected":3`)
	require.NotContains(t, logs, "demo seed imported")
	require.NotContains(t, logs, "demo-products", "the configured path is never logged")
}

func TestSeedDemoFailsWhenTheSeedFails(t *testing.T) {
	boom := errors.New("catalog: the csv header must be exactly name,sku")

	logs, err := runSeed(t, &fakeSeeder{err: boom}, seedFile(t))

	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "demo seed: catalog: the csv header")
	require.Empty(t, logs)
}

func TestSeedDemoFailsWithoutTheFileAndKeepsItsPathPrivate(t *testing.T) {
	s := &fakeSeeder{}
	missing := filepath.Join(t.TempDir(), "nope.csv")

	logs, err := runSeed(t, s, missing)

	require.ErrorIs(t, err, fs.ErrNotExist)
	require.EqualError(t, err, "demo seed: open seed file: no such file or directory")
	require.Zero(t, s.calls, "the catalog is not asked to seed without a file")
	require.Empty(t, logs)
}
