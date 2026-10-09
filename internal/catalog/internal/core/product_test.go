package core_test

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/platform/validation"
)

func dec(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func i64(v int64) *int64 { return &v }

func str(s string) *string { return &s }

func validInput() core.NewProductInput {
	return core.NewProductInput{
		SKU:         "rs-001",
		Name:        "Running Shoes",
		Description: "Lightweight",
		Category:    "Sports",
		Price:       dec("29.99"),
		Stock:       i64(10),
		WeightKg:    dec("0.850"),
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

func TestNewProductRules(t *testing.T) {
	tests := map[string]struct {
		mutate func(*core.NewProductInput)
		field  string
		code   string
	}{
		"sku required":                  {func(in *core.NewProductInput) { in.SKU = "   " }, "sku", "required"},
		"sku too long":                  {func(in *core.NewProductInput) { in.SKU = strings.Repeat("A", 65) }, "sku", "too_long"},
		"sku invalid chars":             {func(in *core.NewProductInput) { in.SKU = "RS 001" }, "sku", "invalid_format"},
		"sku non ascii":                 {func(in *core.NewProductInput) { in.SKU = "RSÑ1" }, "sku", "invalid_format"},
		"name required":                 {func(in *core.NewProductInput) { in.Name = " \t " }, "name", "required"},
		"name too long":                 {func(in *core.NewProductInput) { in.Name = strings.Repeat("ñ", 201) }, "name", "too_long"},
		"description too long":          {func(in *core.NewProductInput) { in.Description = strings.Repeat("x", 2001) }, "description", "too_long"},
		"category required":             {func(in *core.NewProductInput) { in.Category = "  " }, "category", "required"},
		"category too long":             {func(in *core.NewProductInput) { in.Category = strings.Repeat("c", 101) }, "category", "too_long"},
		"price required":                {func(in *core.NewProductInput) { in.Price = nil }, "price", "required"},
		"price zero":                    {func(in *core.NewProductInput) { in.Price = dec("0") }, "price", "not_positive"},
		"price negative":                {func(in *core.NewProductInput) { in.Price = dec("-1") }, "price", "not_positive"},
		"price three decimals":          {func(in *core.NewProductInput) { in.Price = dec("1.001") }, "price", "too_many_decimals"},
		"price above numeric(12,2)":     {func(in *core.NewProductInput) { in.Price = dec("10000000000") }, "price", "out_of_range"},
		"stock required":                {func(in *core.NewProductInput) { in.Stock = nil }, "stock", "required"},
		"stock negative":                {func(in *core.NewProductInput) { in.Stock = i64(-1) }, "stock", "out_of_range"},
		"stock above int32":             {func(in *core.NewProductInput) { in.Stock = i64(2147483648) }, "stock", "out_of_range"},
		"weight required":               {func(in *core.NewProductInput) { in.WeightKg = nil }, "weight_kg", "required"},
		"weight negative":               {func(in *core.NewProductInput) { in.WeightKg = dec("-0.001") }, "weight_kg", "out_of_range"},
		"weight four decimals":          {func(in *core.NewProductInput) { in.WeightKg = dec("0.0001") }, "weight_kg", "too_many_decimals"},
		"weight at numeric(10,3) limit": {func(in *core.NewProductInput) { in.WeightKg = dec("10000000") }, "weight_kg", "out_of_range"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := validInput()
			tt.mutate(&in)

			_, err := core.NewProduct(in)

			require.Equal(t, map[string]string{tt.field: tt.code}, fieldCodes(t, err))
		})
	}
}

func TestNewProductExactLimitsAreAccepted(t *testing.T) {
	in := core.NewProductInput{
		SKU:         strings.Repeat("Z", 64),
		Name:        strings.Repeat("ñ", 200),
		Description: strings.Repeat("d", 2000),
		Category:    strings.Repeat("c", 100),
		Price:       dec("9999999999.99"),
		Stock:       i64(2147483647),
		WeightKg:    dec("9999999.999"),
	}

	p, err := core.NewProduct(in)

	require.NoError(t, err)
	require.Equal(t, "9999999999.99", p.Price.StringFixed(2))
	require.Equal(t, 2147483647, p.Stock)
	require.Equal(t, "9999999.999", p.WeightKg.StringFixed(3))

	in.Price, in.Stock, in.WeightKg = dec("0.01"), i64(0), dec("0")
	_, err = core.NewProduct(in)
	require.NoError(t, err)
}

func TestNewProductNormalizes(t *testing.T) {
	in := validInput()
	in.SKU = "  ab-12_x  "
	in.Name = "  Running Shoes  "
	in.Description = "  Lightweight  "
	in.Category = "  Home   and \t Garden TV "
	in.Price = dec("29.990")

	p, err := core.NewProduct(in)

	require.NoError(t, err)
	require.Equal(t, "AB-12_X", p.SKU)
	require.Equal(t, "Running Shoes", p.Name)
	require.Equal(t, "Lightweight", p.Description)
	require.Equal(t, "Home and Garden TV", p.Category)
	require.Equal(t, "29.99", p.Price.StringFixed(2))
}

func TestNewProductReportsEveryError(t *testing.T) {
	_, err := core.NewProduct(core.NewProductInput{SKU: "bad sku", Price: dec("0"), Stock: i64(-5)})

	require.Equal(t, map[string]string{
		"sku":       "invalid_format",
		"name":      "required",
		"category":  "required",
		"price":     "not_positive",
		"stock":     "out_of_range",
		"weight_kg": "required",
	}, fieldCodes(t, err))
}

func TestChangesEmpty(t *testing.T) {
	require.True(t, core.Changes{}.Empty())
	require.False(t, core.Changes{SKU: str("X")}.Empty())
	require.False(t, core.Changes{Stock: i64(0)}.Empty())
}

func TestValidateChanges(t *testing.T) {
	changes, err := core.ValidateChanges(core.Changes{Name: str("  New name "), Category: str(" Home  Audio "), Stock: i64(0)})
	require.NoError(t, err)
	require.Equal(t, "New name", *changes.Name)
	require.Equal(t, "Home Audio", *changes.Category)
	require.Nil(t, changes.Price)

	_, err = core.ValidateChanges(core.Changes{SKU: str("RS-001"), Name: str(""), Price: dec("1.234")})
	require.Equal(t, map[string]string{
		"sku":   "immutable",
		"name":  "required",
		"price": "too_many_decimals",
	}, fieldCodes(t, err))
}
