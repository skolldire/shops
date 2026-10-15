package app

import (
	"context"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type Tx interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

type Product struct {
	ID    domain.ID
	SKU   string
	Name  string
	Price decimal.Decimal
	Stock int
}

type Stock interface {
	Take(ctx context.Context, items []domain.Quantity, reference string) ([]domain.ReservedLine, error)
	Return(ctx context.Context, items []domain.Quantity, reference string) error
	Product(ctx context.Context, id domain.ID) (Product, error)
}

type Charge struct {
	Reference string
	Token     string
	Amount    decimal.Decimal
	Currency  string
}

type ChargeResult struct {
	Approved    bool
	ProviderRef string
	Reason      string
}

type Payments interface {
	Charge(ctx context.Context, c Charge) (ChargeResult, error)
}

type Purchases interface {
	Insert(ctx context.Context, p domain.Purchase) (domain.Purchase, bool, error)
	AddItems(ctx context.Context, purchaseID domain.ID, items []domain.PurchaseItem) error
	ByKey(ctx context.Context, userID string, key domain.ID) (domain.Purchase, error)
	Get(ctx context.Context, userID string, id domain.ID) (domain.Purchase, error)
	List(ctx context.Context, userID string, p domain.Page) ([]domain.Purchase, int, error)
	Pending(ctx context.Context) ([]domain.Purchase, error)
	Finish(ctx context.Context, id domain.ID, o domain.Outcome) (bool, error)
}

type Carts interface {
	Lines(ctx context.Context, userID string) ([]domain.CartItem, error)
	Add(ctx context.Context, userID string, item domain.CartItem, maxLines int) error
	SetQuantity(ctx context.Context, userID string, productID domain.ID, quantity int) error
	Remove(ctx context.Context, userID string, productID domain.ID) error
	SetPriceSeen(ctx context.Context, userID string, productID domain.ID, price decimal.Decimal) error
	RemovePurchased(ctx context.Context, userID string, items []domain.PurchaseItem) error
}

type CheckoutRecorder interface {
	Checkout(ctx context.Context, outcome string, p *domain.Purchase, cause error)
	Recovery(ctx context.Context, outcome string, n int)
}
