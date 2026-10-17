package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/ordering/internal/app"
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

var (
	errMalformedJSON = fmt.Errorf("%w: malformed JSON or a field of the wrong type", domain.ErrUnreadablePayload)
	errInvalidAmount = fmt.Errorf("%w: an amount that is not a decimal", domain.ErrUnreadablePayload)
	errInvalidFields = fmt.Errorf("%w: missing or inconsistent fields", domain.ErrUnreadablePayload)
	errNoDecoder     = fmt.Errorf("%w: the outbox has no decoder for them", domain.ErrUnsupportedEvent)
)

type eventKey struct {
	kind    domain.EventType
	version int
}

var decoders = map[eventKey]func(raw []byte) (domain.PurchaseEvent, error){
	{domain.EventPurchaseConfirmed, domain.PurchaseEventVersion}: decodeConfirmed,
	{domain.EventPurchaseFailed, domain.PurchaseEventVersion}:    decodeFailed,
}

func decodePayload(kind domain.EventType, version int, aggregateID string, raw []byte) (domain.PurchaseEvent, error) {
	decode, ok := decoders[eventKey{kind: kind, version: version}]
	if !ok {
		return domain.PurchaseEvent{}, errNoDecoder
	}
	e, err := decode(raw)
	if err != nil {
		return domain.PurchaseEvent{}, err
	}
	purchase, err := domain.ParseID(string(e.AggregateID))
	aggregate, errAggregate := domain.ParseID(aggregateID)
	if err != nil || errAggregate != nil || purchase != aggregate || len(e.Items) == 0 {
		return domain.PurchaseEvent{}, errInvalidFields
	}
	for i, it := range e.Items {
		product, err := domain.ParseID(string(it.ProductID))
		if err != nil || it.Quantity < 1 {
			return domain.PurchaseEvent{}, errInvalidFields
		}
		e.Items[i].ProductID = product
	}
	e.AggregateID, e.Type, e.Version = purchase, kind, version
	return e, nil
}

func decodeConfirmed(raw []byte) (domain.PurchaseEvent, error) {
	var in confirmedPayload
	if err := json.Unmarshal(raw, &in); err != nil {
		return domain.PurchaseEvent{}, errMalformedJSON
	}
	total, err := decimal.NewFromString(in.Total)
	if err != nil {
		return domain.PurchaseEvent{}, errInvalidAmount
	}
	e := domain.PurchaseEvent{AggregateID: domain.ID(in.PurchaseID), Total: total, Currency: in.Currency, PaymentRef: in.PaymentRef,
		Items: make([]domain.EventItem, len(in.Items))}
	for i, it := range in.Items {
		price, err := decimal.NewFromString(it.UnitPrice)
		if err != nil {
			return domain.PurchaseEvent{}, errInvalidAmount
		}
		e.Items[i] = domain.EventItem{ProductID: domain.ID(it.ProductID), SKU: it.SKU, Quantity: it.Quantity, UnitPrice: price}
	}
	return e, nil
}

func decodeFailed(raw []byte) (domain.PurchaseEvent, error) {
	var in failedPayload
	if err := json.Unmarshal(raw, &in); err != nil {
		return domain.PurchaseEvent{}, errMalformedJSON
	}
	e := domain.PurchaseEvent{AggregateID: domain.ID(in.PurchaseID), Reason: in.Reason, Items: make([]domain.EventItem, len(in.Items))}
	for i, it := range in.Items {
		e.Items[i] = domain.EventItem{ProductID: domain.ID(it.ProductID), Quantity: it.Quantity}
	}
	return e, nil
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

func (o *Outbox) Claim(ctx context.Context, owner string, lease time.Duration, batch int) (app.ClaimedEvents, error) {
	rows, err := database.Q(ctx, o.pool).Query(ctx,
		`UPDATE outbox SET lease_owner = $1::uuid, lease_until = now() + $2 * interval '1 microsecond'
		 WHERE id IN (
		     SELECT id FROM outbox
		     WHERE published_at IS NULL AND next_attempt_at <= now() AND (lease_until IS NULL OR lease_until < now())
		     ORDER BY id
		     LIMIT $3
		     FOR UPDATE SKIP LOCKED)
		 RETURNING id, event_id::text, occurred_at, attempts, aggregate_id::text, type, version, correlation_id, payload`,
		owner, lease.Microseconds(), batch)
	if err != nil {
		return app.ClaimedEvents{}, fmt.Errorf("ordering: claim outbox events: %w", err)
	}
	var (
		out                                       app.ClaimedEvents
		seq                                       int64
		eventID, aggregateID, kind, correlationID string
		occurredAt                                time.Time
		version, attempts                         int
		payload                                   []byte
	)
	_, err = pgx.ForEachRow(rows, []any{&seq, &eventID, &occurredAt, &attempts, &aggregateID, &kind, &version, &correlationID, &payload}, func() error {
		e, err := decodePayload(domain.EventType(kind), version, aggregateID, payload)
		if err != nil {
			out.Unreadable = append(out.Unreadable, domain.UnreadableEvent{
				Seq: seq, EventID: domain.ID(eventID), Type: domain.EventType(kind), Version: version, Attempts: attempts, Cause: err,
			})
			return nil
		}
		e.AggregateID, e.CorrelationID = domain.ID(aggregateID), correlationID
		out.Readable = append(out.Readable, domain.StoredEvent{Seq: seq, EventID: domain.ID(eventID), OccurredAt: occurredAt, Attempts: attempts, Event: e})
		return nil
	})
	if err != nil {
		return app.ClaimedEvents{}, fmt.Errorf("ordering: claim outbox events: %w", err)
	}
	return out, nil
}

func (o *Outbox) owned(ctx context.Context, sql string, args ...any) (bool, error) {
	tag, err := database.Q(ctx, o.pool).Exec(ctx, sql, args...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (o *Outbox) Renew(ctx context.Context, seq int64, owner string, lease time.Duration) (bool, error) {
	ok, err := o.owned(ctx,
		`UPDATE outbox SET lease_until = now() + $3 * interval '1 microsecond'
		 WHERE id = $1 AND lease_owner = $2::uuid AND published_at IS NULL`,
		seq, owner, lease.Microseconds())
	if err != nil {
		return false, fmt.Errorf("ordering: renew the event lease: %w", err)
	}
	return ok, nil
}

func (o *Outbox) MarkPublished(ctx context.Context, seq int64, owner string) (bool, error) {
	ok, err := o.owned(ctx,
		`UPDATE outbox SET published_at = now(), lease_owner = NULL, lease_until = NULL
		 WHERE id = $1 AND lease_owner = $2::uuid AND published_at IS NULL`,
		seq, owner)
	if err != nil {
		return false, fmt.Errorf("ordering: mark the event published: %w", err)
	}
	return ok, nil
}

func (o *Outbox) Reschedule(ctx context.Context, seq int64, owner string, backoff time.Duration, cause string) (int, bool, error) {
	var attempts int
	err := database.Q(ctx, o.pool).QueryRow(ctx,
		`UPDATE outbox
		 SET attempts = attempts + 1, next_attempt_at = now() + $3 * interval '1 microsecond',
		     last_error = $4, lease_owner = NULL, lease_until = NULL
		 WHERE id = $1 AND lease_owner = $2::uuid AND published_at IS NULL
		 RETURNING attempts`,
		seq, owner, backoff.Microseconds(), cause).Scan(&attempts)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("ordering: reschedule the event: %w", err)
	}
	return attempts, true, nil
}

func (o *Outbox) Stats(ctx context.Context, stuckAfter int) (app.EventStats, error) {
	var stats app.EventStats
	err := database.Q(ctx, o.pool).QueryRow(ctx,
		`SELECT count(*),
		        COALESCE(EXTRACT(EPOCH FROM now() - min(occurred_at)), 0)::float8,
		        count(*) FILTER (WHERE attempts >= $1)
		 FROM outbox WHERE published_at IS NULL`, stuckAfter).Scan(&stats.Pending, &stats.OldestSeconds, &stats.Stuck)
	if err != nil {
		return app.EventStats{}, fmt.Errorf("ordering: read the pending events: %w", err)
	}
	return stats, nil
}
