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

type Stock interface {
	Reserve(ctx context.Context, items []domain.Quantity, reference string) ([]domain.ReservedLine, error)
	Release(ctx context.Context, items []domain.Quantity, reference string) error
	Commit(ctx context.Context, items []domain.Quantity, reference string) error
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

type Payments interface {
	Charge(ctx context.Context, c Charge) (ChargeResult, error)
	Lookup(ctx context.Context, reference string) (PaymentRecord, error)
	Void(ctx context.Context, reference string) (PaymentRecord, error)
}

type Purchases interface {
	Insert(ctx context.Context, p domain.Purchase) (domain.Purchase, bool, error)
	AddItems(ctx context.Context, purchaseID domain.ID, items []domain.PurchaseItem) error
	ByKey(ctx context.Context, userID string, key domain.ID) (domain.Purchase, error)
	Get(ctx context.Context, userID string, id domain.ID) (domain.Purchase, error)
	List(ctx context.Context, userID string, p domain.Page) ([]domain.Purchase, int, error)
	Claim(ctx context.Context, owner string, minAge, lease time.Duration, batch int) ([]domain.Purchase, error)
	Renew(ctx context.Context, id domain.ID, owner string, lease, maxPendingAge time.Duration) (RenewResult, bool, error)
	Reschedule(ctx context.Context, id domain.ID, owner string, backoff time.Duration, cause string) (int, bool, error)
	PendingStats(ctx context.Context, stuckAfter int) (PendingStats, error)
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
