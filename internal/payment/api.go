package payment

import (
	"errors"

	"github.com/shopspring/decimal"
)

type Status string

const (
	StatusApproved Status = "APPROVED"
	StatusDeclined Status = "DECLINED"
	StatusVoided   Status = "VOIDED"
	StatusNotFound Status = "NOT_FOUND"
)

const (
	ReasonCardDeclined        = "card_declined"
	ReasonUnknownPaymentToken = "unknown_payment_token"
	ReasonVoided              = "payment_voided"
)

var (
	ErrOutcomeUnknown    = errors.New("payment: the outcome of the charge is unknown")
	ErrUnavailable       = errors.New("payment: the payment provider is unavailable")
	ErrReferenceConflict = errors.New("payment: the reference was used with other content")
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

type Record struct {
	Status      Status
	ProviderRef string
	Reason      string
	Amount      decimal.Decimal
	Currency    string
}
