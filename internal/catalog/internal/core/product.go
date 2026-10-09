package core

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/platform/validation"
)

const (
	maxSKULength         = 64
	maxNameLength        = 200
	maxDescriptionLength = 2000
	maxCategoryLength    = 100
	maxStock             = 2147483647
	priceDecimals        = 2
	weightDecimals       = 3
)

var (
	skuPattern = regexp.MustCompile(`^[A-Z0-9_-]+$`)
	maxPrice   = decimal.RequireFromString("9999999999.99")
	maxWeight  = decimal.RequireFromString("10000000")
)

type Product struct {
	ID          string
	SKU         string
	Name        string
	Description string
	Category    string
	Price       decimal.Decimal
	Stock       int
	WeightKg    decimal.Decimal
	Version     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type NewProductInput struct {
	SKU         string
	Name        string
	Description string
	Category    string
	Price       *decimal.Decimal
	Stock       *int64
	WeightKg    *decimal.Decimal
}

type Changes struct {
	SKU         *string
	Name        *string
	Description *string
	Category    *string
	Price       *decimal.Decimal
	Stock       *int64
	WeightKg    *decimal.Decimal
}

func (c Changes) Empty() bool {
	return c.Name == nil && c.Description == nil && c.Category == nil &&
		c.Price == nil && c.Stock == nil && c.WeightKg == nil
}

func NewProduct(in NewProductInput) (Product, error) {
	var errs validation.Errors
	p := Product{
		SKU:         checkSKU(&errs, in.SKU),
		Name:        checkName(&errs, in.Name),
		Description: checkDescription(&errs, in.Description),
		Category:    checkCategory(&errs, in.Category),
		Price:       checkPrice(&errs, in.Price),
		Stock:       checkStock(&errs, in.Stock),
		WeightKg:    checkWeight(&errs, in.WeightKg),
	}
	if err := errs.Err(); err != nil {
		return Product{}, err
	}
	return p, nil
}

func ValidateChanges(c Changes) (Changes, error) {
	var (
		errs validation.Errors
		out  Changes
	)
	if c.SKU != nil {
		errs.Add("sku", validation.CodeImmutable, "cannot be changed")
	}
	if c.Name != nil {
		out.Name = ptr(checkName(&errs, *c.Name))
	}
	if c.Description != nil {
		out.Description = ptr(checkDescription(&errs, *c.Description))
	}
	if c.Category != nil {
		out.Category = ptr(checkCategory(&errs, *c.Category))
	}
	if c.Price != nil {
		out.Price = ptr(checkPrice(&errs, c.Price))
	}
	if c.Stock != nil {
		out.Stock = ptr(int64(checkStock(&errs, c.Stock)))
	}
	if c.WeightKg != nil {
		out.WeightKg = ptr(checkWeight(&errs, c.WeightKg))
	}
	if err := errs.Err(); err != nil {
		return Changes{}, err
	}
	return out, nil
}

func checkSKU(errs *validation.Errors, raw string) string {
	sku := strings.ToUpper(strings.TrimSpace(raw))
	switch {
	case sku == "":
		errs.Add("sku", validation.CodeRequired, "is required")
	case utf8.RuneCountInString(sku) > maxSKULength:
		errs.Add("sku", validation.CodeTooLong, "must be at most 64 characters")
	case !skuPattern.MatchString(sku):
		errs.Add("sku", validation.CodeInvalidFormat, "may only contain letters, digits, '-' and '_'")
	}
	return sku
}

func checkName(errs *validation.Errors, raw string) string {
	name := strings.TrimSpace(raw)
	switch {
	case name == "":
		errs.Add("name", validation.CodeRequired, "is required")
	case utf8.RuneCountInString(name) > maxNameLength:
		errs.Add("name", validation.CodeTooLong, "must be at most 200 characters")
	}
	return name
}

func checkDescription(errs *validation.Errors, raw string) string {
	description := strings.TrimSpace(raw)
	if utf8.RuneCountInString(description) > maxDescriptionLength {
		errs.Add("description", validation.CodeTooLong, "must be at most 2000 characters")
	}
	return description
}

func checkCategory(errs *validation.Errors, raw string) string {
	category := strings.Join(strings.Fields(raw), " ")
	switch {
	case category == "":
		errs.Add("category", validation.CodeRequired, "is required")
	case utf8.RuneCountInString(category) > maxCategoryLength:
		errs.Add("category", validation.CodeTooLong, "must be at most 100 characters")
	}
	return category
}

func checkPrice(errs *validation.Errors, price *decimal.Decimal) decimal.Decimal {
	switch {
	case price == nil:
		errs.Add("price", validation.CodeRequired, "is required")
		return decimal.Decimal{}
	case !price.IsPositive():
		errs.Add("price", validation.CodeNotPositive, "must be greater than 0")
	case !hasAtMostDecimals(*price, priceDecimals):
		errs.Add("price", validation.CodeTooManyDecimals, "must have at most 2 decimals")
	case price.GreaterThan(maxPrice):
		errs.Add("price", validation.CodeOutOfRange, "must be at most 9999999999.99")
	}
	return *price
}

func checkStock(errs *validation.Errors, stock *int64) int {
	switch {
	case stock == nil:
		errs.Add("stock", validation.CodeRequired, "is required")
		return 0
	case *stock < 0 || *stock > maxStock:
		errs.Add("stock", validation.CodeOutOfRange, "must be between 0 and 2147483647")
		return 0
	}
	return int(*stock)
}

func checkWeight(errs *validation.Errors, weight *decimal.Decimal) decimal.Decimal {
	switch {
	case weight == nil:
		errs.Add("weight_kg", validation.CodeRequired, "is required")
		return decimal.Decimal{}
	case weight.IsNegative() || weight.GreaterThanOrEqual(maxWeight):
		errs.Add("weight_kg", validation.CodeOutOfRange, "must be at least 0 and less than 10000000")
	case !hasAtMostDecimals(*weight, weightDecimals):
		errs.Add("weight_kg", validation.CodeTooManyDecimals, "must have at most 3 decimals")
	}
	return *weight
}

func hasAtMostDecimals(d decimal.Decimal, places int32) bool {
	return d.Equal(d.Truncate(places))
}

func ptr[T any](v T) *T {
	return &v
}
