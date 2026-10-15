package app

import (
	"context"
	"errors"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type CartLine struct {
	Item         domain.CartItem
	Product      Product
	Unavailable  bool
	LowStock     bool
	PriceChanged bool
}

type CartView struct {
	Lines          []CartLine
	EstimatedTotal decimal.Decimal
}

func (v CartView) HasWarnings() bool {
	for _, l := range v.Lines {
		if l.Unavailable || l.LowStock || l.PriceChanged {
			return true
		}
	}
	return false
}

type Cart struct {
	carts Carts
	stock Stock
}

func NewCart(carts Carts, stock Stock) (*Cart, error) {
	if carts == nil || stock == nil {
		return nil, errors.New("ordering: cart requires carts and stock")
	}
	return &Cart{carts: carts, stock: stock}, nil
}

func (c *Cart) View(ctx context.Context, userID string) (CartView, error) {
	items, err := c.carts.Lines(ctx, userID)
	if err != nil {
		return CartView{}, err
	}
	view := CartView{Lines: make([]CartLine, 0, len(items)), EstimatedTotal: decimal.Zero}
	for _, it := range items {
		line := CartLine{Item: it}
		p, err := c.stock.Product(ctx, it.ProductID)
		switch {
		case errors.Is(err, ErrProductUnavailable):
			line.Unavailable = true
		case err != nil:
			return CartView{}, err
		default:
			line.Product = p
			line.LowStock = p.Available < it.Quantity
			line.PriceChanged = !p.Price.Equal(it.UnitPriceSeen)
		}
		view.EstimatedTotal = view.EstimatedTotal.Add(it.UnitPriceSeen.Mul(decimal.NewFromInt(int64(it.Quantity))))
		view.Lines = append(view.Lines, line)
	}
	return view, nil
}

func (c *Cart) Count(ctx context.Context, userID string) (int, error) {
	items, err := c.carts.Lines(ctx, userID)
	if err != nil {
		return 0, err
	}
	return len(items), nil
}

func (c *Cart) Add(ctx context.Context, userID, rawProductID string, quantity int) error {
	id, err := c.line(userID, rawProductID, quantity)
	if err != nil {
		return err
	}
	p, err := c.stock.Product(ctx, id)
	if err != nil {
		return err
	}
	return c.carts.Add(ctx, userID, domain.CartItem{ProductID: id, Quantity: quantity, UnitPriceSeen: p.Price}, domain.MaxCartLines)
}

func (c *Cart) SetQuantity(ctx context.Context, userID, rawProductID string, quantity int) error {
	id, err := c.line(userID, rawProductID, quantity)
	if err != nil {
		return err
	}
	return c.carts.SetQuantity(ctx, userID, id, quantity)
}

func (c *Cart) Remove(ctx context.Context, userID, rawProductID string) error {
	id, err := c.line(userID, rawProductID, 1)
	if err != nil {
		return err
	}
	return c.carts.Remove(ctx, userID, id)
}

func (c *Cart) AcceptCurrentPrice(ctx context.Context, userID, rawProductID string) error {
	id, err := c.line(userID, rawProductID, 1)
	if err != nil {
		return err
	}
	p, err := c.stock.Product(ctx, id)
	if err != nil {
		return err
	}
	return c.carts.SetPriceSeen(ctx, userID, id, p.Price)
}

func (c *Cart) CheckoutInput(ctx context.Context, userID, idempotencyKey, paymentToken string) (domain.CheckoutInput, error) {
	items, err := c.carts.Lines(ctx, userID)
	if err != nil {
		return domain.CheckoutInput{}, err
	}
	in := domain.CheckoutInput{IdempotencyKey: idempotencyKey, PaymentToken: paymentToken, Items: make([]domain.CheckoutItemInput, len(items))}
	for i, it := range items {
		price := it.UnitPriceSeen
		in.Items[i] = domain.CheckoutItemInput{ProductID: string(it.ProductID), Quantity: it.Quantity, ExpectedUnitPrice: &price}
	}
	return in, nil
}

func (c *Cart) line(userID, rawProductID string, quantity int) (domain.ID, error) {
	if userID == "" {
		return "", domain.ErrUserRequired
	}
	id, err := domain.ParseID(rawProductID)
	if err != nil {
		return "", err
	}
	if err := domain.CheckQuantity(quantity); err != nil {
		return "", err
	}
	return id, nil
}
