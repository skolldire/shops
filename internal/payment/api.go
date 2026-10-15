package payment

import (
	"errors"

	"github.com/shopspring/decimal"
)

type Status string

const (
	StatusApproved Status = "APPROVED"
	StatusDeclined Status = "DECLINED"
)

const (
	ReasonCardDeclined        = "card_declined"
	ReasonUnknownPaymentToken = "unknown_payment_token"
)

var (
	ErrOutcomeUnknown = errors.New("payment: the outcome of the charge is unknown")
	ErrUnavailable    = errors.New("payment: the payment provider is unavailable")
)

type ChargeRequest struct {
	Reference string
	Token     string
	Amount    decimal.Decimal
	Currency  string
}

type ChargeResult struct {
	Status      Status
	ProviderRef string
	Reason      string
}
