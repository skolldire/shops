package app

import (
	"context"
	"errors"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type PurchasePage struct {
	Items    []domain.Purchase
	Page     int
	PageSize int
	Total    int
}

type Queries struct {
	purchases Purchases
}

func NewQueries(purchases Purchases) (*Queries, error) {
	if purchases == nil {
		return nil, errors.New("ordering: queries require purchases")
	}
	return &Queries{purchases: purchases}, nil
}

func (q *Queries) Purchase(ctx context.Context, userID, rawID string) (domain.Purchase, error) {
	id, err := domain.ParseID(rawID)
	if err != nil || userID == "" {
		return domain.Purchase{}, ErrPurchaseNotFound
	}
	return q.purchases.Get(ctx, userID, id)
}

func (q *Queries) Purchases(ctx context.Context, userID string, page, pageSize int) (PurchasePage, error) {
	if userID == "" {
		return PurchasePage{}, domain.ErrUserRequired
	}
	p, err := domain.NewPage(page, pageSize)
	if err != nil {
		return PurchasePage{}, err
	}
	items, total, err := q.purchases.List(ctx, userID, p)
	if err != nil {
		return PurchasePage{}, err
	}
	return PurchasePage{Items: items, Page: p.Page, PageSize: p.PageSize, Total: total}, nil
}
