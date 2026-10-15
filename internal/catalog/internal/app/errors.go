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
	ErrStockInvariant      = errors.New("catalog: the reservation does not match the stock")
)

type InsufficientStockError struct {
	ProductID string
	Available int
}

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("catalog: insufficient stock for product %s: %d available", e.ProductID, e.Available)
}

type StockInvariantError struct {
	ProductID string
	Quantity  int
}

func (e *StockInvariantError) Error() string {
	return fmt.Sprintf("catalog: product %s has fewer than %d reserved units to settle", e.ProductID, e.Quantity)
}

func (e *StockInvariantError) Is(target error) bool {
	return target == ErrStockInvariant
}

type ProductUnavailableError struct {
	ProductID string
}

func (e *ProductUnavailableError) Error() string {
	return fmt.Sprintf("catalog: product %s is unavailable", e.ProductID)
}

func (e *ProductUnavailableError) Is(target error) bool {
	return target == ErrProductUnavailable
}
