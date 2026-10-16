package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

type MovementPage struct {
	Items    []domain.Movement
	Page     int
	PageSize int
	Total    int
}

type Inventory struct {
	store StockStore
}

func NewInventory(store StockStore) (*Inventory, error) {
	if store == nil {
		return nil, errors.New("catalog: inventory requires a stock store")
	}
	return &Inventory{store: store}, nil
}

func (i *Inventory) ReserveStock(ctx context.Context, items []domain.StockRequest, reference string) ([]domain.StockLine, error) {
	sorted, err := sortedRequests(items)
	if err != nil {
		return nil, err
	}
	return i.store.ReserveStock(ctx, sorted, reference)
}

func (i *Inventory) ReleaseStock(ctx context.Context, items []domain.StockRequest, reference string) error {
	sorted, err := sortedRequests(items)
	if err != nil {
		return err
	}
	return i.store.ReleaseStock(ctx, sorted, reference)
}

func (i *Inventory) CommitStock(ctx context.Context, items []domain.StockRequest, reference string) error {
	sorted, err := sortedRequests(items)
	if err != nil {
		return err
	}
	return i.store.CommitStock(ctx, sorted, reference)
}

func (i *Inventory) Movements(ctx context.Context, productID string, p domain.Paging) (MovementPage, error) {
	if !idPattern.MatchString(productID) {
		return MovementPage{}, ErrNotFound
	}
	items, total, err := i.store.Movements(ctx, strings.ToLower(productID), p)
	if err != nil {
		return MovementPage{}, err
	}
	return MovementPage{Items: items, Page: p.Page, PageSize: p.PageSize, Total: total}, nil
}

func sortedRequests(items []domain.StockRequest) ([]domain.StockRequest, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: no items", ErrInvalidStockRequest)
	}
	sorted := make([]domain.StockRequest, len(items))
	for n, it := range items {
		switch {
		case !idPattern.MatchString(it.ProductID):
			return nil, fmt.Errorf("%w: item %d has a malformed product id", ErrInvalidStockRequest, n)
		case it.Quantity < 1:
			return nil, fmt.Errorf("%w: item %d has a quantity below 1", ErrInvalidStockRequest, n)
		}
		sorted[n] = domain.StockRequest{ProductID: strings.ToLower(it.ProductID), Quantity: it.Quantity}
	}
	slices.SortFunc(sorted, func(a, b domain.StockRequest) int { return cmp.Compare(a.ProductID, b.ProductID) })
	for n := 1; n < len(sorted); n++ {
		if sorted[n].ProductID == sorted[n-1].ProductID {
			return nil, fmt.Errorf("%w: product %s is repeated", ErrInvalidStockRequest, sorted[n].ProductID)
		}
	}
	return sorted, nil
}
