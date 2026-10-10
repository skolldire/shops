package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/core"
)

func TestParseDecimalAccepts(t *testing.T) {
	tests := map[string]string{
		"0":                    "0",
		"29.99":                "29.99",
		"0.001":                "0.001",
		"-5":                   "-5",
		"9999999999.99":        "9999999999.99",
		"1234567890123.123456": "1234567890123.123456",
		"  42.5  ":             "42.5",
	}
	for raw, want := range tests {
		d, err := core.ParseDecimal(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, d.String(), raw)
	}
}

func TestParseDecimalRejects(t *testing.T) {
	for _, raw := range []string{
		"1e3", "1E3", "1e100000000", "1e-100000000", "2.5e-1", "+1", ".5", "5.",
		"1,000.00", "NaN", "Infinity", "", "  ",
		strings.Repeat("1", 14), "1." + strings.Repeat("1", 7), "0x10", "1_000", "--1",
	} {
		_, err := core.ParseDecimal(raw)
		require.ErrorIs(t, err, core.ErrInvalidDecimal, raw)
		require.EqualError(t, err, "must be a plain decimal number such as 29.99", raw)
	}
}

func TestParseDecimalRejectsHugeExponentsQuickly(t *testing.T) {
	for _, raw := range []string{"1e100000000", "1e-100000000"} {
		done := make(chan error, 1)
		go func() {
			_, err := core.ParseDecimal(raw)
			done <- err
		}()
		select {
		case err := <-done:
			require.ErrorIs(t, err, core.ErrInvalidDecimal, raw)
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("%s took longer than 100ms to reject", raw)
		}
	}
}
