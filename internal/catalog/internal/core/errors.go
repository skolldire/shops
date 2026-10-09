package core

import "errors"

var (
	ErrNotFound        = errors.New("catalog: product not found")
	ErrSKUTaken        = errors.New("catalog: sku already taken")
	ErrVersionConflict = errors.New("catalog: version conflict")
)
