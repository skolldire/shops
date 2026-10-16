package ordering

import (
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/platform/validation"
)

var (
	ErrInvalidIdempotencyKey = errors.New("ordering: the idempotency key must be a UUID")
	ErrInvalidID             = errors.New("ordering: invalid id")
	ErrIdempotencyConflict   = errors.New("ordering: the idempotency key was already used with another request")
	ErrPurchaseNotFound      = errors.New("ordering: purchase not found")
	ErrProductUnavailable    = errors.New("ordering: product unavailable")
	ErrInsufficientStock     = errors.New("ordering: insufficient stock")
	ErrPriceChanged          = errors.New("ordering: price changed")
	ErrCartFull              = fmt.Errorf("ordering: the cart already has %d lines", MaxCartLines)
	ErrTryAgain              = errors.New("ordering: the database is busy, try again")
)

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

type Status string

const (
	StatusPending   Status = "PENDING"
	StatusConfirmed Status = "CONFIRMED"
	StatusFailed    Status = "FAILED"
)

type PurchaseItem struct {
	ProductID string
	SKU       string
	Name      string
	UnitPrice decimal.Decimal
	Quantity  int
	LineTotal decimal.Decimal
}

type Purchase struct {
	ID            string
	Status        Status
	Currency      string
	Total         decimal.Decimal
	PaymentRef    string
	FailureReason string
	CreatedAt     time.Time
	Items         []PurchaseItem
}

type ResultKind int

const (
	Created ResultKind = iota + 1
	Replayed
	InProgress
)

type CheckoutResult struct {
	Purchase Purchase
	Kind     ResultKind
}

type PurchasePage struct {
	Items    []Purchase
	Page     int
	PageSize int
	Total    int
}

type CartLine struct {
	ProductID     string
	SKU           string
	Name          string
	Quantity      int
	UnitPriceSeen decimal.Decimal
	CurrentPrice  decimal.Decimal
	Available     int
	Unavailable   bool
	LowStock      bool
	PriceChanged  bool
}

type Cart struct {
	Lines          []CartLine
	EstimatedTotal decimal.Decimal
	HasWarnings    bool
}

type Budgets struct {
	Reserve  time.Duration
	Charge   time.Duration
	Finalize time.Duration
}

type RecoveryConfig struct {
	Interval           time.Duration
	BatchSize          int
	MinAge             time.Duration
	Lease              time.Duration
	DBTimeout          time.Duration
	BaseBackoff        time.Duration
	MaxBackoff         time.Duration
	StuckAfterAttempts int
	MaxPendingAge      time.Duration
}

type PendingPurchase struct {
	ID    string
	Cause error
}

type RecoveryReport struct {
	Confirmed    int
	Declined     int
	Voided       int
	Skipped      int
	StillPending []PendingPurchase
}
