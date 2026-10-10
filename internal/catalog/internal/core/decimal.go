package core

import (
	"errors"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
)

var (
	ErrInvalidDecimal = errors.New("must be a plain decimal number such as 29.99")
	decimalPattern    = regexp.MustCompile(`^-?[0-9]{1,13}(\.[0-9]{1,6})?$`)
)

func ParseDecimal(raw string) (decimal.Decimal, error) {
	s := strings.TrimSpace(raw)
	if !decimalPattern.MatchString(s) {
		return decimal.Decimal{}, ErrInvalidDecimal
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Decimal{}, ErrInvalidDecimal
	}
	return d, nil
}
