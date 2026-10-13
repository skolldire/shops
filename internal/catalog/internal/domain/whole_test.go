package domain_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

func TestParseWholeNumberAccepts(t *testing.T) {
	tests := map[string]int64{
		"0":           0,
		"25":          25,
		"-5":          -5,
		"007":         7,
		"  42  ":      42,
		"2147483647":  2147483647,
		"-2147483648": -2147483648,
	}
	for raw, want := range tests {
		n, err := domain.ParseWholeNumber(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, n, raw)
	}
}

func TestParseWholeNumberRejectsOtherFormats(t *testing.T) {
	for _, raw := range []string{
		"", "  ", "+5", "1.0", "1.5", "1e3", "1E3", "1,000", "1_000", "0x10", "ten", "--1", "5-", "1 2", "NaN", "null", `"5"`, "٥",
	} {
		_, err := domain.ParseWholeNumber(raw)
		require.ErrorIs(t, err, domain.ErrInvalidWholeNumber, raw)
		require.EqualError(t, err, "must be a whole number such as 25", raw)
	}
}

func TestWholeNumberErrorCode(t *testing.T) {
	require.Equal(t, "invalid_format", domain.WholeNumberErrorCode(domain.ErrInvalidWholeNumber))
	require.Equal(t, "out_of_range", domain.WholeNumberErrorCode(domain.ErrWholeNumberOutOfRange))
}

func TestParseWholeNumberRejectsValuesBeyond32Bits(t *testing.T) {
	for _, raw := range []string{"2147483648", "-2147483649", "9223372036854775807", "99999999999999999999", "-99999999999999999999"} {
		_, err := domain.ParseWholeNumber(raw)
		require.ErrorIs(t, err, domain.ErrWholeNumberOutOfRange, raw)
		require.NotErrorIs(t, err, domain.ErrInvalidWholeNumber, raw)
		require.EqualError(t, err, "must be between -2147483648 and 2147483647", raw)
	}
}
