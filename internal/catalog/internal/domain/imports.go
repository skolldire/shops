package domain

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

type ImportMode string

const (
	ImportAdd     ImportMode = "add"
	ImportReplace ImportMode = "replace"
)

var ErrInvalidImportMode = errors.New("catalog: the import mode must be add or replace")

func ParseImportMode(raw string) (ImportMode, error) {
	switch mode := ImportMode(raw); mode {
	case ImportAdd, ImportReplace:
		return mode, nil
	case "":
		return ImportReplace, nil
	}
	return "", ErrInvalidImportMode
}

type ImportStatus string

const (
	ImportApplying ImportStatus = "APPLYING"
	ImportApplied  ImportStatus = "APPLIED"
)

type ImportStamp struct {
	CreatedAt time.Time
	AppliedAt time.Time
}

type ImportChange struct {
	SKU         string
	StockBefore int
	StockAfter  int
	PriceBefore decimal.Decimal
	PriceAfter  decimal.Decimal
}
