package app

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type Tx interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

type Product struct {
	ID        domain.ID
	SKU       string
	Name      string
	Price     decimal.Decimal
	Available int
}

type StockReservation interface {
	Reserve(ctx context.Context, items []domain.Quantity, reference string) ([]domain.ReservedLine, error)
}

type StockSettlement interface {
	Release(ctx context.Context, items []domain.Quantity, reference string) error
	Commit(ctx context.Context, items []domain.Quantity, reference string) error
}

type ProductReader interface {
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

type PaymentStatus string

const (
	PaymentApproved PaymentStatus = "APPROVED"
	PaymentDeclined PaymentStatus = "DECLINED"
	PaymentVoided   PaymentStatus = "VOIDED"
	PaymentNotFound PaymentStatus = "NOT_FOUND"
)

type PaymentRecord struct {
	Status      PaymentStatus
	ProviderRef string
	Reason      string
	Amount      decimal.Decimal
	Currency    string
}

type PaymentCharger interface {
	Charge(ctx context.Context, c Charge) (ChargeResult, error)
}

type Payments interface {
	PaymentCharger
	Lookup(ctx context.Context, reference string) (PaymentRecord, error)
	Void(ctx context.Context, reference string) (PaymentRecord, error)
}

type PurchaseCreation interface {
	Insert(ctx context.Context, p domain.Purchase) (domain.Purchase, bool, error)
	AddItems(ctx context.Context, purchaseID domain.ID, items []domain.PurchaseItem) error
	ByKey(ctx context.Context, userID string, key domain.ID) (domain.Purchase, error)
}

type PurchaseReader interface {
	Get(ctx context.Context, userID string, id domain.ID) (domain.Purchase, error)
	List(ctx context.Context, userID string, p domain.Page) ([]domain.Purchase, int, error)
}

type PendingPurchaseLister interface {
	Claim(ctx context.Context, owner string, minAge, lease time.Duration, batch int) ([]domain.Purchase, error)
}

type PurchaseLease interface {
	Renew(ctx context.Context, id domain.ID, owner string, lease, maxPendingAge time.Duration) (RenewResult, bool, error)
	Reschedule(ctx context.Context, id domain.ID, owner string, backoff time.Duration, cause string) (int, bool, error)
}

type PendingPurchaseStats interface {
	PendingStats(ctx context.Context, stuckAfter int) (PendingStats, error)
}

type PurchaseFinisher interface {
	Finish(ctx context.Context, id domain.ID, o domain.Outcome) (bool, error)
}

type CartEditor interface {
	Lines(ctx context.Context, userID string) ([]domain.CartItem, error)
	Add(ctx context.Context, userID string, item domain.CartItem, maxLines int) error
	SetQuantity(ctx context.Context, userID string, productID domain.ID, quantity int) error
	Remove(ctx context.Context, userID string, productID domain.ID) error
	SetPriceSeen(ctx context.Context, userID string, productID domain.ID, price decimal.Decimal) error
}

type PurchasedCartRemover interface {
	RemovePurchased(ctx context.Context, userID string, items []domain.PurchaseItem) error
}

type RenewResult struct {
	Attempts int
	Overdue  bool
}

type PendingStats struct {
	Pending            int
	OldestSeconds      float64
	Stuck              int
	StuckReservedUnits int
}

type RecoveryAttempt struct {
	PurchaseID domain.ID
	Attempts   int
	Outcome    string
	Cause      error
	StuckAfter int
}

type RecoveryResolution struct {
	PurchaseID domain.ID
	Attempts   int
	Result     string
	Cause      error
}

type CheckoutRecorder interface {
	Checkout(ctx context.Context, outcome string, p *domain.Purchase, cause error)
	RecoveryAttempt(ctx context.Context, a RecoveryAttempt)
	RecoveryResolution(ctx context.Context, r RecoveryResolution)
	RecoveryGauges(ctx context.Context, s PendingStats)
}

type CheckoutStock interface {
	StockReservation
	StockSettlement
}

type CheckoutPurchases interface {
	PurchaseCreation
	PurchaseFinisher
	Get(ctx context.Context, userID string, id domain.ID) (domain.Purchase, error)
}

type RecoveryPurchases interface {
	PendingPurchases
	PurchaseFinisher
}

type Purchases interface {
	CheckoutPurchases
	PurchaseReader
	PendingPurchases
}

type Carts interface {
	CartEditor
	PurchasedCartRemover
}

type PendingPurchases interface {
	PendingPurchaseLister
	PurchaseLease
	PendingPurchaseStats
}
