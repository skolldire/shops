package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering"
	"github.com/skolldire/shops/internal/platform/logger"
)

type fakeRecoverer struct {
	report ordering.RecoveryReport
	err    error
	calls  int
}

func (f *fakeRecoverer) RecoverPending(context.Context) (ordering.RecoveryReport, error) {
	f.calls++
	return f.report, f.err
}

func runRecovery(t *testing.T, r *fakeRecoverer) (string, error) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	err = recoverPurchases(context.Background(), log, r)
	return logs.String(), err
}

func TestRecoveryLogsItsSummaryAndEachStillPendingPurchase(t *testing.T) {
	r := &fakeRecoverer{report: ordering.RecoveryReport{Confirmed: 2, Declined: 1, StillPending: []ordering.PendingPurchase{
		{ID: "p-1", Cause: errors.New("ordering: charge: payment provider unavailable")},
		{ID: "p-2", Cause: errors.New("ordering: recovery time budget exhausted")},
	}}}
	logs, err := runRecovery(t, r)
	require.NoError(t, err, "purchases that stay pending do not stop the startup")
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	require.Len(t, lines, 3)
	require.Contains(t, lines[0], `"severity":"INFO"`)
	require.Contains(t, lines[0], `"message":"pending purchases recovered"`)
	require.Contains(t, lines[0], `"confirmed":2`)
	require.Contains(t, lines[0], `"declined":1`)
	require.Contains(t, lines[0], `"still_pending":2`)
	require.Contains(t, lines[1], `"severity":"WARN"`)
	require.Contains(t, lines[1], `"purchase_id":"p-1"`)
	require.Contains(t, lines[2], `"purchase_id":"p-2"`)
}

func TestRecoveryFailureStopsTheStartup(t *testing.T) {
	r := &fakeRecoverer{err: errors.New("ordering: list pending purchases: connection refused")}
	logs, err := runRecovery(t, r)
	require.EqualError(t, err, "purchase recovery: ordering: list pending purchases: connection refused")
	require.Empty(t, logs)
}
