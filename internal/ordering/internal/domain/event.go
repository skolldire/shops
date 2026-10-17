package domain

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

type EventType string

const (
	EventPurchaseConfirmed EventType = "PurchaseConfirmed"
	EventPurchaseFailed    EventType = "PurchaseFailed"

	AggregatePurchase    = "Purchase"
	PurchaseEventVersion = 1
)

var ErrNoEventForOutcome = errors.New("ordering: only a confirmed or failed purchase has an outcome event")

type EventItem struct {
	ProductID ID
	SKU       string
	Quantity  int
	UnitPrice decimal.Decimal
}

type PurchaseEvent struct {
	Type          EventType
	AggregateID   ID
	CorrelationID string
	Version       int
	Items         []EventItem
	Total         decimal.Decimal
	Currency      string
	PaymentRef    string
	Reason        string
}

type StoredEvent struct {
	Seq        int64
	EventID    ID
	OccurredAt time.Time
	Attempts   int
	Event      PurchaseEvent
}

func NewPurchaseEvent(p Purchase, o Outcome) (PurchaseEvent, error) {
	e := PurchaseEvent{
		AggregateID:   p.ID,
		CorrelationID: "purchase:" + string(p.ID),
		Version:       PurchaseEventVersion,
		Items:         make([]EventItem, len(p.Items)),
	}
	for i, it := range p.Items {
		e.Items[i] = EventItem{ProductID: it.ProductID, SKU: it.SKU, Quantity: it.Quantity, UnitPrice: it.UnitPrice}
	}
	switch o.Status {
	case StatusConfirmed:
		e.Type, e.Total, e.Currency, e.PaymentRef = EventPurchaseConfirmed, p.Total, p.Currency, o.PaymentRef
	case StatusFailed:
		e.Type, e.Reason = EventPurchaseFailed, o.FailureReason
	default:
		return PurchaseEvent{}, ErrNoEventForOutcome
	}
	return e, nil
}
