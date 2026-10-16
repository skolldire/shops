package domain

import (
	"errors"
	"regexp"
	"strings"
)

var (
	ErrInvalidID = errors.New("ordering: invalid id")
	idPattern    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

type ID string

func ParseID(raw string) (ID, error) {
	if !idPattern.MatchString(raw) {
		return "", ErrInvalidID
	}
	return ID(strings.ToLower(raw)), nil
}
