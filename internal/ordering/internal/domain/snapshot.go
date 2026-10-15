package domain

import (
	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/platform/validation"
)

const CodePriceChanged = "price_changed"

type ReservedLine struct {
	ProductID ID
	SKU       string
	Name      string
	Price     decimal.Decimal
	Quantity  int
}

func Snapshot(r CheckoutRequest, lines []ReservedLine) ([]PurchaseItem, error) {
	byID := make(map[ID]ReservedLine, len(lines))
	for _, l := range lines {
		byID[l.ProductID] = l
	}
	var changed validation.Errors
	items := make([]PurchaseItem, 0, len(r.Items))
	for _, it := range r.Items {
		line, ok := byID[it.ProductID]
		if !ok || line.Quantity != it.Quantity {
			return nil, ErrReservationMismatch
		}
		if !line.Price.Equal(it.ExpectedUnitPrice) {
			changed.Add(ItemField(it.Index, "expected_unit_price"), CodePriceChanged,
				"the current price is "+line.Price.StringFixed(priceDecimals))
			continue
		}
		items = append(items, PurchaseItem{
			ProductID: it.ProductID, SKU: line.SKU, Name: line.Name, UnitPrice: line.Price, Quantity: it.Quantity,
			LineTotal: line.Price.Mul(decimal.NewFromInt(int64(it.Quantity))),
		})
	}
	if err := changed.Err(); err != nil {
		return nil, &PriceChangedError{Errors: changed}
	}
	return items, nil
}

type PriceChangedError struct {
	Errors validation.Errors
}

func (e *PriceChangedError) Error() string {
	return "ordering: prices changed: " + e.Errors.Error()
}
