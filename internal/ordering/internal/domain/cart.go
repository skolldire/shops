package domain

import (
	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/platform/validation"
)

const MaxCartLines = 50

type CartItem struct {
	ProductID     ID
	Quantity      int
	UnitPriceSeen decimal.Decimal
}

func CheckQuantity(qty int) error {
	if qty < 1 || qty > MaxQuantity {
		return validation.Errors{{Field: "quantity", Code: validation.CodeOutOfRange, Message: "must be between 1 and 99"}}
	}
	return nil
}
