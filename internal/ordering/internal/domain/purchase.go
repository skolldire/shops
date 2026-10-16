package domain

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

const Currency = "USD"

type Status string

const (
	StatusPending   Status = "PENDING"
	StatusConfirmed Status = "CONFIRMED"
	StatusFailed    Status = "FAILED"
)

var ErrInvalidTransition = errors.New("ordering: invalid purchase transition")

type PurchaseItem struct {
	ProductID ID
	SKU       string
	Name      string
	UnitPrice decimal.Decimal
	Quantity  int
	LineTotal decimal.Decimal
}

type Purchase struct {
	ID             ID
	UserID         string
	IdempotencyKey ID
	RequestHash    string
	Status         Status
	Currency       string
	Total          decimal.Decimal
	PaymentToken   string
	PaymentRef     string
	FailureReason  string
	CreatedAt      time.Time
	Items          []PurchaseItem
}

type Outcome struct {
	Status        Status
	PaymentRef    string
	FailureReason string
}

func Confirmed(paymentRef string) Outcome {
	return Outcome{Status: StatusConfirmed, PaymentRef: paymentRef}
}

func Failed(reason string) Outcome {
	return Outcome{Status: StatusFailed, FailureReason: reason}
}

func NewPendingPurchase(r CheckoutRequest) Purchase {
	return Purchase{
		UserID:         r.UserID,
		IdempotencyKey: r.IdempotencyKey,
		RequestHash:    r.Hash,
		Status:         StatusPending,
		Currency:       r.Currency,
		Total:          r.Total,
		PaymentToken:   r.PaymentToken,
	}
}

func (p Purchase) Finished() bool {
	return p.Status != StatusPending
}

func (p *Purchase) Apply(o Outcome) error {
	if p.Status != StatusPending || (o.Status != StatusConfirmed && o.Status != StatusFailed) {
		return ErrInvalidTransition
	}
	p.Status, p.PaymentRef, p.FailureReason = o.Status, o.PaymentRef, o.FailureReason
	return nil
}

func (p Purchase) Quantities() []Quantity {
	out := make([]Quantity, len(p.Items))
	for i, it := range p.Items {
		out[i] = Quantity{ProductID: it.ProductID, Quantity: it.Quantity}
	}
	return out
}

type Quantity struct {
	ProductID ID
	Quantity  int
}
