package catalog

import (
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

var (
	ErrNotFound        = errors.New("catalog: product not found")
	ErrSKUTaken        = errors.New("catalog: sku already taken")
	ErrVersionConflict = errors.New("catalog: version conflict")

	ErrInvalidHeader = errors.New("catalog: the csv header must be exactly name,sku,description,category,price,stock,weight_kg")
	ErrInvalidCSV    = errors.New("catalog: invalid csv")
	ErrTooManyRows   = errors.New("catalog: the csv has too many rows")
	ErrFileTooLarge  = errors.New("catalog: the csv file is too large")
)

type InvalidCSVError struct {
	Line   int
	Reason string
}

func (e *InvalidCSVError) Error() string {
	return fmt.Sprintf("catalog: invalid csv on line %d: %s", e.Line, e.Reason)
}

func (e *InvalidCSVError) Is(target error) bool {
	return target == ErrInvalidCSV
}

type ImportLimits struct {
	MaxBytes int64
	MaxRows  int
}

type ImportOptions struct {
	FileName string
}

type ImportFile struct {
	Name   string
	Bytes  int64
	SHA256 string
}

type RowError struct {
	Line    int
	SKU     string
	Field   string
	Code    string
	Message string
}

type ImportReport struct {
	File      ImportFile
	Records   int
	Skipped   int
	Created   int
	Updated   int
	Unchanged int
	Rejected  int
	Errors    []RowError
}

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

type SearchParams struct {
	Q        string
	Category string
	MinPrice string
	MaxPrice string
	InStock  string
	Sort     string
	Page     string
	PageSize string
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
