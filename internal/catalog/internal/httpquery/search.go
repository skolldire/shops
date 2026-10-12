package httpquery

import (
	"net/url"
	"strconv"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/platform/validation"
)

type Params struct {
	Q        string
	Category string
	MinPrice string
	MaxPrice string
	InStock  string
	Sort     string
	Page     string
	PageSize string
}

func FromValues(values url.Values) Params {
	return Params{
		Q:        values.Get("q"),
		Category: values.Get("category"),
		MinPrice: values.Get("min_price"),
		MaxPrice: values.Get("max_price"),
		InStock:  values.Get("in_stock"),
		Sort:     values.Get("sort"),
		Page:     values.Get("page"),
		PageSize: values.Get("page_size"),
	}
}

func ParseSearch(values url.Values) (core.Search, error) {
	return ParseParams(FromValues(values))
}

func ParseParams(p Params) (core.Search, error) {
	var format validation.Errors
	in := core.SearchInput{
		Q:        p.Q,
		Category: p.Category,
		Sort:     p.Sort,
		MinPrice: decimalParam(&format, "min_price", p.MinPrice),
		MaxPrice: decimalParam(&format, "max_price", p.MaxPrice),
		InStock:  boolParam(&format, "in_stock", p.InStock),
		Page:     positiveParam(&format, "page", p.Page),
		PageSize: positiveParam(&format, "page_size", p.PageSize),
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
