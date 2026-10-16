package app

import (
	"errors"
	"fmt"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/validation"
)

const (
	CodeProductUnavailable = "product_unavailable"
	CodeInsufficientStock  = "insufficient_stock"
)

var (
	ErrIdempotencyConflict   = errors.New("ordering: the idempotency key was already used with another request")
	ErrPurchaseNotFound      = errors.New("ordering: purchase not found")
	ErrProductUnavailable    = errors.New("ordering: product unavailable")
	ErrInsufficientStock     = errors.New("ordering: insufficient stock")
	ErrPriceChanged          = errors.New("ordering: price changed")
	ErrCartFull              = fmt.Errorf("ordering: the cart already has %d lines", domain.MaxCartLines)
	ErrBusy                  = errors.New("ordering: the database is busy, try again")
	ErrPaymentOutcomeUnknown = errors.New("ordering: the outcome of the payment is unknown")
	ErrPaymentUnavailable    = errors.New("ordering: the payment provider is unavailable")
	ErrPaymentConflict       = errors.New("ordering: the payment reference was used with other content")
)

type UnavailableProductError struct {
	ProductID domain.ID
}

func (e *UnavailableProductError) Error() string {
	return fmt.Sprintf("ordering: product %s is unavailable", e.ProductID)
}

func (e *UnavailableProductError) Is(target error) bool {
	return target == ErrProductUnavailable
}

type InsufficientStockError struct {
	ProductID domain.ID
	Available int
}

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("ordering: product %s has %d units available", e.ProductID, e.Available)
}

func (e *InsufficientStockError) Is(target error) bool {
	return target == ErrInsufficientStock
}

type RejectedError struct {
	Reason error
	Errors validation.Errors
}

func (e *RejectedError) Error() string {
	return e.Reason.Error() + ": " + e.Errors.Error()
}

func (e *RejectedError) Is(target error) bool {
	return target == e.Reason
}
