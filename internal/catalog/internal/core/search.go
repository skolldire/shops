package core

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/platform/validation"
)

const (
	maxQueryLength  = 100
	defaultPageSize = 20
	maxPageSize     = 100
	maxPage         = 100000
)

type SortField string

const (
	SortName      SortField = "name"
	SortPrice     SortField = "price"
	SortCreatedAt SortField = "created_at"
)

type Search struct {
	Q          string
	Category   string
	MinPrice   *decimal.Decimal
	MaxPrice   *decimal.Decimal
	InStock    bool
	Sort       SortField
	Descending bool
	Page       int
	PageSize   int
}

func (s Search) Offset() int {
	return (s.Page - 1) * s.PageSize
}

type SearchInput struct {
	Q        string
	Category string
	MinPrice *decimal.Decimal
	MaxPrice *decimal.Decimal
	InStock  bool
	Sort     string
	Page     int
	PageSize int
}

type Page struct {
	Items    []Product
	Page     int
	PageSize int
	Total    int
}

func ParseSearch(values url.Values) (Search, error) {
	var (
		errs validation.Errors
		in   = SearchInput{Q: values.Get("q"), Category: values.Get("category"), Sort: values.Get("sort")}
	)
	in.MinPrice = parseDecimal(&errs, "min_price", values.Get("min_price"))
	in.MaxPrice = parseDecimal(&errs, "max_price", values.Get("max_price"))
	if raw := values.Get("in_stock"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			errs.Add("in_stock", validation.CodeInvalidFormat, "must be true or false")
		}
		in.InStock = v
	}
	in.Page = parsePositive(&errs, "page", values.Get("page"))
	in.PageSize = parsePositive(&errs, "page_size", values.Get("page_size"))

	return newSearch(in, errs)
}

func NewSearch(in SearchInput) (Search, error) {
	return newSearch(in, nil)
}

func newSearch(in SearchInput, errs validation.Errors) (Search, error) {
	s := Search{
		Q:        strings.TrimSpace(in.Q),
		Category: strings.Join(strings.Fields(in.Category), " "),
		MinPrice: in.MinPrice,
		MaxPrice: in.MaxPrice,
		InStock:  in.InStock,
		Page:     in.Page,
		PageSize: in.PageSize,
	}
	if utf8.RuneCountInString(s.Q) > maxQueryLength {
		errs.Add("q", validation.CodeTooLong, "must be at most 100 characters")
	}
	checkBound(&errs, "min_price", s.MinPrice)
	checkBound(&errs, "max_price", s.MaxPrice)
	if s.MinPrice != nil && s.MaxPrice != nil && !s.MinPrice.IsNegative() && !s.MinPrice.GreaterThan(maxPrice) &&
		s.MinPrice.GreaterThan(*s.MaxPrice) {
		errs.Add("min_price", validation.CodeOutOfRange, "must not be greater than max_price")
	}
	s.Sort, s.Descending = parseSort(&errs, in.Sort)

	switch {
	case s.Page == 0:
		s.Page = 1
	case s.Page < 0 || s.Page > maxPage:
		errs.Add("page", validation.CodeOutOfRange, "must be between 1 and 100000")
	}
	switch {
	case s.PageSize == 0:
		s.PageSize = defaultPageSize
	case s.PageSize < 0 || s.PageSize > maxPageSize:
		errs.Add("page_size", validation.CodeOutOfRange, "must be between 1 and 100")
	}

	if err := errs.Err(); err != nil {
		return Search{}, err
	}
	return s, nil
}

func parseSort(errs *validation.Errors, raw string) (SortField, bool) {
	if raw == "" {
		return SortName, false
	}
	desc := strings.HasPrefix(raw, "-")
	switch field := SortField(strings.TrimPrefix(raw, "-")); field {
	case SortName, SortPrice, SortCreatedAt:
		return field, desc
	}
	errs.Add("sort", validation.CodeInvalidFormat, "must be one of name, price or created_at, optionally prefixed with '-'")
	return SortName, false
}

func parseDecimal(errs *validation.Errors, field, raw string) *decimal.Decimal {
	if raw == "" {
		return nil
	}
	d, err := ParseDecimal(raw)
	if err != nil {
		errs.Add(field, validation.CodeInvalidFormat, err.Error())
		return nil
	}
	return &d
}

func parsePositive(errs *validation.Errors, field, raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	var numErr *strconv.NumError
	switch {
	case errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange):
		errs.Add(field, validation.CodeOutOfRange, "is too large")
		return 1
	case err != nil:
		errs.Add(field, validation.CodeInvalidFormat, "must be an integer")
		return 1
	case n < 1:
		errs.Add(field, validation.CodeOutOfRange, "must be at least 1")
		return 1
	}
	return int(n)
}

func checkBound(errs *validation.Errors, field string, d *decimal.Decimal) {
	switch {
	case d == nil:
	case d.IsNegative():
		errs.Add(field, validation.CodeOutOfRange, "must not be negative")
	case d.GreaterThan(maxPrice):
		errs.Add(field, validation.CodeOutOfRange, "must be at most 9999999999.99")
	}
}
