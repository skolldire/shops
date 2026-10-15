package observe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/app/apptest"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/catalog/internal/observe"
	"github.com/skolldire/shops/internal/platform/logger"
)

func newReporter(t *testing.T) (*observe.FailureReporter, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	reporter, err := observe.NewFailureReporter(log)
	require.NoError(t, err)
	return reporter, &logs
}

func entries(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry), line)
		out = append(out, entry)
	}
	return out
}

func TestBatchRetriedIsAWarningBecauseTheImportRecovers(t *testing.T) {
	reporter, logs := newReporter(t)

	reporter.BatchRetried(t.Context(), 500, errors.New("deadlock detected"))

	got := entries(t, logs)
	require.Len(t, got, 1)
	require.Equal(t, "WARN", got[0]["severity"])
	require.Equal(t, "catalog: import batch failed, retrying row by row", got[0]["message"])
	require.EqualValues(t, 500, got[0]["rows"])
	require.Equal(t, "deadlock detected", got[0]["cause"])
	require.Equal(t, "catalog", got[0]["component"])
}

func TestRowRejectedIsAnErrorWithTheCauseAndTheRow(t *testing.T) {
	reporter, logs := newReporter(t)

	reporter.RowRejected(t.Context(), 42, "RS-001", errors.New("check constraint violated"))

	got := entries(t, logs)
	require.Len(t, got, 1)
	require.Equal(t, "ERROR", got[0]["severity"])
	require.Equal(t, "catalog: import row rejected by the database: check constraint violated", got[0]["message"])
	require.EqualValues(t, 42, got[0]["line"])
	require.Equal(t, "RS-001", got[0]["sku"])
}

func TestNewFailureReporterRequiresALogger(t *testing.T) {
	_, err := observe.NewFailureReporter(nil)
	require.EqualError(t, err, "catalog: import failure reporter requires a logger")
}

type flakyStore struct {
	failSKU string
}

func (f flakyStore) Upsert(_ context.Context, _ domain.ImportMode, products []domain.Product, _ string) (app.UpsertResult, error) {
	if len(products) > 1 {
		return app.UpsertResult{}, errors.New("batch exploded")
	}
	out := map[string]app.UpsertOutcome{}
	for _, p := range products {
		if p.SKU == f.failSKU {
			return app.UpsertResult{}, errors.New("row exploded")
		}
		out[p.SKU] = app.OutcomeCreated
	}
	return app.UpsertResult{Outcomes: out}, nil
}

func importRow(t *testing.T, line int, sku string) app.ImportRow {
	t.Helper()
	price, err := domain.ParseDecimal("9.99")
	require.NoError(t, err)
	weight, err := domain.ParseDecimal("0.3")
	require.NoError(t, err)
	stock := int64(1)
	return app.ImportRow{Line: line, Product: domain.NewProductInput{
		SKU: sku, Name: "Item", Category: "Things", Price: &price, Stock: &stock, WeightKg: &weight,
	}}
}

func TestABatchRecoveryIsLoggedOnceAndEachRejectedRowOnce(t *testing.T) {
	reporter, logs := newReporter(t)
	counters := &fakeCounters{}
	recorder, err := observe.NewImportRecorder(counters)
	require.NoError(t, err)
	importer, err := app.NewImporter(flakyStore{failSKU: "B-1"}, apptest.NewLog(), apptest.Tx{}, recorder, reporter)
	require.NoError(t, err)

	report, err := importer.Import(t.Context(), app.ImportRequest{File: app.ImportFile{Name: "products.csv"}, Mode: domain.ImportReplace, Rows: []app.ImportRow{
		importRow(t, 2, "A-1"), importRow(t, 3, "B-1"), importRow(t, 4, "C-1"),
	}})

	require.NoError(t, err)
	require.Equal(t, []int{2, 1}, []int{report.Created, report.Rejected})
	recoveries, rejections := 0, 0
	for _, entry := range entries(t, logs) {
		switch entry["message"] {
		case "catalog: import batch failed, retrying row by row":
			recoveries++
			require.Equal(t, "WARN", entry["severity"])
		case "catalog: import row rejected by the database: row exploded":
			rejections++
			require.Equal(t, "ERROR", entry["severity"])
			require.Equal(t, "B-1", entry["sku"])
		default:
			t.Fatalf("unexpected log line: %v", entry)
		}
	}
	require.Equal(t, 1, recoveries, "the recovery of the batch is logged once")
	require.Equal(t, 1, rejections, "the terminal rejection of the row is logged once")
	require.Equal(t, map[string]int64{
		"import_rows_total{outcome=created}":   2,
		"import_rows_total{outcome=rejected}":  1,
		"import_rows_total{outcome=updated}":   0,
		"import_rows_total{outcome=unchanged}": 0,
		"import_rows_total{outcome=skipped}":   0,
	}, counters.counters, "the retried batch does not count its rows twice")
}
