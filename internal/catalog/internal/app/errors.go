package app

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound            = errors.New("catalog: product not found")
	ErrSKUTaken            = errors.New("catalog: sku already taken")
	ErrVersionConflict     = errors.New("catalog: version conflict")
	ErrProductUnavailable  = errors.New("catalog: product unavailable")
	ErrInvalidStockRequest = errors.New("catalog: invalid stock request")
)

type InsufficientStockError struct {
	ProductID string
	Available int
}

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("catalog: insufficient stock for product %s: %d available", e.ProductID, e.Available)
}
