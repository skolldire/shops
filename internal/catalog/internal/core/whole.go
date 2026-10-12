package core

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/skolldire/shops/internal/platform/validation"
)

var (
	ErrInvalidWholeNumber    = errors.New("must be a whole number such as 25")
	ErrWholeNumberOutOfRange = errors.New("must be between -2147483648 and 2147483647")
	wholeNumberPattern       = regexp.MustCompile(`^-?[0-9]+$`)
)

func WholeNumberErrorCode(err error) string {
	if errors.Is(err, ErrWholeNumberOutOfRange) {
		return validation.CodeOutOfRange
	}
	return validation.CodeInvalidFormat
}

func ParseWholeNumber(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if !wholeNumberPattern.MatchString(s) {
		return 0, ErrInvalidWholeNumber
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, ErrWholeNumberOutOfRange
	}
	return n, nil
}
