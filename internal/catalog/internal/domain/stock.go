package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

type StockRequest struct {
	ProductID string
	Quantity  int
}

type StockLine struct {
	ProductID string
	SKU       string
	Name      string
	Price     decimal.Decimal
	Quantity  int
}

type Movement struct {
	ID         int64
	ProductID  string
	Delta      int
	StockAfter int
	Reason     string
	Reference  string
	DBUser     string
	TxID       string
	CreatedAt  time.Time
}
