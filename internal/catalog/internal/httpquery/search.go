package httpquery

import (
	"net/url"
	"strconv"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/platform/validation"
)

func ParseSearch(values url.Values) (core.Search, error) {
	var format validation.Errors
	in := core.SearchInput{
		Q:        values.Get("q"),
		Category: values.Get("category"),
		Sort:     values.Get("sort"),
		MinPrice: decimalParam(&format, "min_price", values.Get("min_price")),
		MaxPrice: decimalParam(&format, "max_price", values.Get("max_price")),
		InStock:  boolParam(&format, "in_stock", values.Get("in_stock")),
		Page:     positiveParam(&format, "page", values.Get("page")),
		PageSize: positiveParam(&format, "page_size", values.Get("page_size")),
	}
	search, err := core.NewSearch(in)
	if merged := validation.Merge(format, err); merged != nil {
		return core.Search{}, merged
	}
	return search, nil
}

func decimalParam(errs *validation.Errors, field, raw string) *decimal.Decimal {
	if raw == "" {
		return nil
	}
	d, err := core.ParseDecimal(raw)
	if err != nil {
		errs.Add(field, validation.CodeInvalidFormat, err.Error())
		return nil
	}
	return &d
}

func boolParam(errs *validation.Errors, field, raw string) bool {
	if raw == "" {
		return false
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		errs.Add(field, validation.CodeInvalidFormat, "must be true or false")
	}
	return v
}

func positiveParam(errs *validation.Errors, field, raw string) int {
	if raw == "" {
		return 0
	}
	n, err := core.ParseWholeNumber(raw)
	switch {
	case err != nil:
		errs.Add(field, core.WholeNumberErrorCode(err), err.Error())
		return 1
	case n < 1:
		errs.Add(field, validation.CodeOutOfRange, "must be at least 1")
		return 1
	}
	return int(n)
}
