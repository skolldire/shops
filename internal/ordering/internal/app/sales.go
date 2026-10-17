package app

import (
	"context"
	"errors"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

const SalesConsumer = "sales_projection"

type SalesProjection struct {
	tx    Tx
	inbox Inbox
	sales SalesProjectionStore
}

func NewSalesProjection(tx Tx, inbox Inbox, sales SalesProjectionStore) (*SalesProjection, error) {
	if tx == nil || inbox == nil || sales == nil {
		return nil, errors.New("ordering: the sales projection requires a transaction manager, an inbox and a store")
	}
	return &SalesProjection{tx: tx, inbox: inbox, sales: sales}, nil
}

func (s *SalesProjection) Supports() []domain.EventKind {
	return domain.SupportedEvents()
}

func (s *SalesProjection) Handle(ctx context.Context, e domain.StoredEvent) (bool, error) {
	if !domain.SupportedEvent(e.Event.Type, e.Event.Version) {
		return false, domain.ErrUnsupportedEvent
	}
	applied := false
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		applied = false
		recorded, err := s.inbox.Record(ctx, SalesConsumer, e.EventID)
		if err != nil || !recorded {
			return err
		}
		if e.Event.Type == domain.EventPurchaseConfirmed {
			for _, it := range e.Event.Items {
				if err := s.sales.AddSale(ctx, it.ProductID, it.Quantity); err != nil {
					return err
				}
			}
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}
