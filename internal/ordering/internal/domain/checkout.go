package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/platform/validation"
)

const (
	MaxItems          = 50
	MaxQuantity       = 99
	maxTokenLength    = 64
	priceDecimals     = 2
	CodeDuplicate     = "duplicate"
	hashSchemaVersion = "checkout-v1"
)

var itemsRange = "must have between 1 and " + strconv.Itoa(MaxItems) + " items"

var (
	ErrInvalidIdempotencyKey = errors.New("ordering: the idempotency key must be a UUID")
	ErrUserRequired          = errors.New("ordering: a user is required")
	maxTotal                 = decimal.RequireFromString("9999999999.99")
)

type CheckoutItemInput struct {
	ProductID         string
	Quantity          int
	ExpectedUnitPrice *decimal.Decimal
}

type CheckoutInput struct {
	IdempotencyKey string
	Items          []CheckoutItemInput
	PaymentToken   string
}

type CheckoutItem struct {
	Index             int
	ProductID         ID
	Quantity          int
	ExpectedUnitPrice decimal.Decimal
}

type CheckoutRequest struct {
	UserID         string
	IdempotencyKey ID
	Items          []CheckoutItem
	PaymentToken   string
	Currency       string
	Total          decimal.Decimal
	Hash           string
}

func NewCheckoutRequest(userID string, in CheckoutInput) (CheckoutRequest, error) {
	if userID == "" {
		return CheckoutRequest{}, ErrUserRequired
	}
	key, err := ParseID(in.IdempotencyKey)
	if err != nil {
		return CheckoutRequest{}, ErrInvalidIdempotencyKey
	}
	req := CheckoutRequest{UserID: userID, IdempotencyKey: key, PaymentToken: in.PaymentToken, Currency: Currency}

	var errs validation.Errors
	switch n := len(in.Items); {
	case n == 0:
		errs.Add("items", validation.CodeRequired, itemsRange)
	case n > MaxItems:
		errs.Add("items", validation.CodeOutOfRange, itemsRange)
	}
	seen := make(map[ID]int, len(in.Items))
	valid := true
	for i, raw := range in.Items {
		item, ok := checkItem(&errs, i, raw)
		if first, dup := seen[item.ProductID]; ok && dup {
			errs.Add(ItemField(i, "product_id"), CodeDuplicate, "repeats the product of items["+strconv.Itoa(first)+"]")
			ok = false
		}
		if ok {
			seen[item.ProductID] = i
		}
		valid = valid && ok
		req.Items = append(req.Items, item)
	}
	switch {
	case in.PaymentToken == "":
		errs.Add("payment_token", validation.CodeRequired, "is required")
	case len([]rune(in.PaymentToken)) > maxTokenLength:
		errs.Add("payment_token", validation.CodeTooLong, "must be at most 64 characters")
	}
	if valid && len(errs) == 0 {
		req.Total = totalOf(req.Items)
		if req.Total.GreaterThan(maxTotal) {
			errs.Add("total", validation.CodeOutOfRange, "the order total must be at most 9999999999.99")
		}
	}
	if err := errs.Err(); err != nil {
		return CheckoutRequest{}, err
	}
	slices.SortFunc(req.Items, func(a, b CheckoutItem) int { return strings.Compare(string(a.ProductID), string(b.ProductID)) })
	req.Hash = requestHash(req)
	return req, nil
}

func checkItem(errs *validation.Errors, i int, raw CheckoutItemInput) (CheckoutItem, bool) {
	item := CheckoutItem{Index: i, Quantity: raw.Quantity}
	before := len(*errs)
	id, err := ParseID(raw.ProductID)
	if err != nil {
		errs.Add(ItemField(i, "product_id"), validation.CodeInvalidFormat, "must be a UUID such as 2b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b")
	}
	item.ProductID = id
	if raw.Quantity < 1 || raw.Quantity > MaxQuantity {
		errs.Add(ItemField(i, "quantity"), validation.CodeOutOfRange, "must be between 1 and 99")
	}
	price := raw.ExpectedUnitPrice
	switch {
	case price == nil:
		errs.Add(ItemField(i, "expected_unit_price"), validation.CodeRequired, "is required")
	case !price.IsPositive():
		errs.Add(ItemField(i, "expected_unit_price"), validation.CodeNotPositive, "must be greater than 0")
	case !price.Equal(price.Truncate(priceDecimals)):
		errs.Add(ItemField(i, "expected_unit_price"), validation.CodeTooManyDecimals, "must have at most 2 decimals")
	case price.GreaterThan(maxTotal):
		errs.Add(ItemField(i, "expected_unit_price"), validation.CodeOutOfRange, "must be at most 9999999999.99")
	default:
		item.ExpectedUnitPrice = *price
	}
	return item, len(*errs) == before
}

func ItemField(i int, name string) string {
	return "items[" + strconv.Itoa(i) + "]." + name
}

func totalOf(items []CheckoutItem) decimal.Decimal {
	total := decimal.Zero
	for _, it := range items {
		total = total.Add(it.ExpectedUnitPrice.Mul(decimal.NewFromInt(int64(it.Quantity))))
	}
	return total
}

func (r CheckoutRequest) Quantities() []Quantity {
	out := make([]Quantity, len(r.Items))
	for i, it := range r.Items {
		out[i] = Quantity{ProductID: it.ProductID, Quantity: it.Quantity}
	}
	return out
}

func requestHash(r CheckoutRequest) string {
	var b strings.Builder
	write := func(s string) {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	write(hashSchemaVersion)
	write(r.Currency)
	write(r.PaymentToken)
	write(strconv.Itoa(len(r.Items)))
	for _, it := range r.Items {
		write(string(it.ProductID))
		write(strconv.Itoa(it.Quantity))
		write(it.ExpectedUnitPrice.StringFixed(priceDecimals))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
