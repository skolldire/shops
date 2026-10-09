package catalog

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

var (
	ErrNotFound        = errors.New("catalog: product not found")
	ErrSKUTaken        = errors.New("catalog: sku already taken")
	ErrVersionConflict = errors.New("catalog: version conflict")
)

type Product struct {
	ID          string
	SKU         string
	Name        string
	Description string
	Category    string
	Price       decimal.Decimal
	Stock       int
	WeightKg    decimal.Decimal
	Version     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type CreateInput struct {
	SKU         string
	Name        string
	Description string
	Category    string
	Price       *decimal.Decimal
	Stock       *int64
	WeightKg    *decimal.Decimal
}

type UpdateInput struct {
	Version     int
	SKU         *string
	Name        *string
	Description *string
	Category    *string
	Price       *decimal.Decimal
	Stock       *int64
	WeightKg    *decimal.Decimal
}

type SearchQuery struct {
	Q        string
	Category string
	MinPrice *decimal.Decimal
	MaxPrice *decimal.Decimal
	InStock  bool
	Sort     string
	Page     int
	PageSize int
}

type Page struct {
	Items    []Product
	Page     int
	PageSize int
	Total    int
}
