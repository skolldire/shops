package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/database"
)

type Outbox struct {
	pool *pgxpool.Pool
}

func NewOutbox(pool *pgxpool.Pool) (*Outbox, error) {
	if pool == nil {
		return nil, errors.New("ordering: the outbox requires a pool")
	}
	return &Outbox{pool: pool}, nil
}

type confirmedItem struct {
	ProductID string `json:"product_id"`
	SKU       string `json:"sku"`
	Quantity  int    `json:"quantity"`
	UnitPrice string `json:"unit_price"`
}

type confirmedPayload struct {
	PurchaseID string          `json:"purchase_id"`
	Total      string          `json:"total"`
	Currency   string          `json:"currency"`
	PaymentRef string          `json:"payment_ref"`
	Items      []confirmedItem `json:"items"`
}

type failedItem struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

type failedPayload struct {
	PurchaseID string       `json:"purchase_id"`
	Reason     string       `json:"reason"`
	Items      []failedItem `json:"items"`
}

func encodePayload(e domain.PurchaseEvent) ([]byte, error) {
	switch e.Type {
	case domain.EventPurchaseConfirmed:
		out := confirmedPayload{PurchaseID: string(e.AggregateID), Total: e.Total.StringFixed(2), Currency: e.Currency,
			PaymentRef: e.PaymentRef, Items: make([]confirmedItem, len(e.Items))}
		for i, it := range e.Items {
			out.Items[i] = confirmedItem{ProductID: string(it.ProductID), SKU: it.SKU, Quantity: it.Quantity, UnitPrice: it.UnitPrice.StringFixed(2)}
		}
		return json.Marshal(out)
	case domain.EventPurchaseFailed:
		out := failedPayload{PurchaseID: string(e.AggregateID), Reason: e.Reason, Items: make([]failedItem, len(e.Items))}
		for i, it := range e.Items {
			out.Items[i] = failedItem{ProductID: string(it.ProductID), Quantity: it.Quantity}
		}
		return json.Marshal(out)
	}
	return nil, fmt.Errorf("ordering: unknown event type %q", e.Type)
}

func (o *Outbox) Append(ctx context.Context, e domain.PurchaseEvent) error {
	payload, err := encodePayload(e)
	if err != nil {
		return err
	}
	_, err = database.Q(ctx, o.pool).Exec(ctx,
		`INSERT INTO outbox (aggregate_type, aggregate_id, type, version, correlation_id, payload)
		 VALUES ($1, $2::uuid, $3, $4, $5, $6)`,
		domain.AggregatePurchase, string(e.AggregateID), string(e.Type), e.Version, e.CorrelationID, payload)
	if err != nil {
		return fmt.Errorf("ordering: append the purchase event: %w", err)
	}
	return nil
}
