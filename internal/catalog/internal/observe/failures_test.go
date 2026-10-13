package observe_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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
