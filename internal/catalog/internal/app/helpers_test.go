package app_test

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/validation"
)

func dec(t *testing.T, s string) *decimal.Decimal {
	t.Helper()
	d, err := domain.ParseDecimal(s)
	require.NoError(t, err)
	return &d
}

func str(s string) *string { return &s }

func validInput(t *testing.T) domain.NewProductInput {
	t.Helper()
	stock := int64(10)
	return domain.NewProductInput{
		SKU:         "rs-001",
		Name:        "Running Shoes",
		Description: "Lightweight",
		Category:    "Sports",
		Price:       dec(t, "29.99"),
		Stock:       &stock,
		WeightKg:    dec(t, "0.850"),
	}
}

func fieldCodes(t *testing.T, err error) map[string]string {
	t.Helper()
	var errs validation.Errors
	require.ErrorAs(t, err, &errs)
	codes := map[string]string{}
	for _, fe := range errs {
		require.NotContains(t, codes, fe.Field, "one error per field")
		require.NotEmpty(t, fe.Message)
		codes[fe.Field] = fe.Code
	}
	return codes
}
