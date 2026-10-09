package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrorsBeforeTheLoggerGoToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := run([]string{"-config", filepath.Join(t.TempDir(), "missing.yaml")}, &stdout)
	report(&stderr, err)

	require.Error(t, err)
	require.Empty(t, stdout.String())
	require.Contains(t, stderr.String(), "config: read")
}

func TestErrorsAfterTheLoggerAreLoggedOnce(t *testing.T) {
	t.Setenv("DB_PASSWORD", "test-password")
	t.Setenv("DB_PORT", "1")
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("METRICS_ADDR", "127.0.0.1:1")
	var stdout, stderr bytes.Buffer

	err := run([]string{"-config", "../../config/config.local.yaml"}, &stdout)
	report(&stderr, err)

	require.Error(t, err)
	require.Empty(t, stderr.String(), "already logged errors must not be printed again")
	failures := 0
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry), line)
		if entry["severity"] == "ERROR" {
			failures++
			require.Equal(t, "database: ping: dial failed: connection refused", entry["message"])
		}
	}
	require.Equal(t, 1, failures)
}
