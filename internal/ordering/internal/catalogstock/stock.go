package catalogstock

import (
	"context"
	"errors"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type Catalog interface {
	ReserveStock(ctx context.Context, items []catalog.StockRequest, reference string) ([]catalog.StockLine, error)
	ReleaseStock(ctx context.Context, items []catalog.StockRequest, reference string) error
	CommitStock(ctx context.Context, items []catalog.StockRequest, reference string) error
	Get(ctx context.Context, id string) (catalog.Product, error)
}

type Stock struct {
	catalog Catalog
}

func New(c Catalog) (*Stock, error) {
	if c == nil {
		return nil, errors.New("ordering: stock requires the catalog")
	}
	return &Stock{catalog: c}, nil
}

func (s *Stock) Reserve(ctx context.Context, items []domain.Quantity, reference string) ([]domain.ReservedLine, error) {
	lines, err := s.catalog.ReserveStock(ctx, requests(items), reference)
	if err != nil {
		return nil, translate(err)
	}
	out := make([]domain.ReservedLine, len(lines))
	for i, l := range lines {
		out[i] = domain.ReservedLine{ProductID: domain.ID(l.ProductID), SKU: l.SKU, Name: l.Name, Price: l.Price, Quantity: l.Quantity}
	}
	return out, nil
}

func (s *Stock) Release(ctx context.Context, items []domain.Quantity, reference string) error {
	return translate(s.catalog.ReleaseStock(ctx, requests(items), reference))
}

func (s *Stock) Commit(ctx context.Context, items []domain.Quantity, reference string) error {
	return translate(s.catalog.CommitStock(ctx, requests(items), reference))
}

func (s *Stock) Product(ctx context.Context, id domain.ID) (app.Product, error) {
	p, err := s.catalog.Get(ctx, string(id))
	if errors.Is(err, catalog.ErrNotFound) {
		return app.Product{}, &app.UnavailableProductError{ProductID: id}
	}
	if err != nil {
		return app.Product{}, err
	}
	return app.Product{ID: domain.ID(p.ID), SKU: p.SKU, Name: p.Name, Price: p.Price, Available: p.Available}, nil
}

func requests(items []domain.Quantity) []catalog.StockRequest {
	out := make([]catalog.StockRequest, len(items))
	for i, it := range items {
		out[i] = catalog.StockRequest{ProductID: string(it.ProductID), Quantity: it.Quantity}
	}
	return out
}

func translate(err error) error {
	var (
		unavailable *catalog.ProductUnavailableError
		short       *catalog.InsufficientStockError
	)
	switch {
	case errors.As(err, &unavailable):
		return &app.UnavailableProductError{ProductID: domain.ID(unavailable.ProductID)}
	case errors.As(err, &short):
		return &app.InsufficientStockError{ProductID: domain.ID(short.ProductID), Available: short.Available}
	}
	return err
}
