package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/validation"
)

type Budgets struct {
	Reserve  time.Duration
	Charge   time.Duration
	Finalize time.Duration
}

type ResultKind int

const (
	Created ResultKind = iota + 1
	Replayed
	InProgress
)

type CheckoutResult struct {
	Purchase domain.Purchase
	Kind     ResultKind
}

type Checkout struct {
	finalizer
	payments Payments
	rec      CheckoutRecorder
	budgets  Budgets
}

var errKeyExists = errors.New("ordering: the idempotency key already exists")

func NewCheckout(tx Tx, stock Stock, payments Payments, purchases Purchases, carts Carts, rec CheckoutRecorder, b Budgets) (*Checkout, error) {
	switch {
	case tx == nil, stock == nil, payments == nil, purchases == nil, carts == nil, rec == nil:
		return nil, errors.New("ordering: checkout requires a transaction manager, stock, payments, purchases, carts and a recorder")
	case b.Reserve <= 0, b.Charge <= 0, b.Finalize <= 0:
		return nil, errors.New("ordering: checkout time budgets must be positive")
	}
	return &Checkout{
		finalizer: finalizer{tx: tx, purchases: purchases, stock: stock, carts: carts},
		payments:  payments, rec: rec, budgets: b,
	}, nil
}

func (c *Checkout) Checkout(ctx context.Context, userID string, in domain.CheckoutInput) (CheckoutResult, error) {
	req, err := domain.NewCheckoutRequest(userID, in)
	if err != nil {
		outcome := OutcomeInvalid
		if errors.Is(err, domain.ErrUserRequired) {
			outcome = OutcomeError
		}
		c.rec.Checkout(ctx, outcome, nil, nil)
		return CheckoutResult{}, err
	}

	p, inserted, err := c.reserve(ctx, req)
	if err != nil {
		c.rec.Checkout(ctx, reserveOutcome(err), nil, nil)
		return CheckoutResult{}, err
	}
	if !inserted {
		return c.replay(ctx, req, p)
	}

	p, cause := c.settle(ctx, p)
	c.rec.Checkout(ctx, settledOutcome(p), &p, cause)
	return CheckoutResult{Purchase: p, Kind: Created}, nil
}

func (c *Checkout) reserve(ctx context.Context, req domain.CheckoutRequest) (domain.Purchase, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.budgets.Reserve)
	defer cancel()

	var created domain.Purchase
	err := c.tx.WithinTx(ctx, func(ctx context.Context) error {
		p, ok, err := c.purchases.Insert(ctx, domain.NewPendingPurchase(req))
		if err != nil {
			return err
		}
		if !ok {
			return errKeyExists
		}
		lines, err := c.stock.Take(ctx, req.Quantities(), string(p.ID))
		if err != nil {
			return rejection(req, err)
		}
		items, err := domain.Snapshot(req, lines)
		var changed *domain.PriceChangedError
		if errors.As(err, &changed) {
			return &RejectedError{Reason: ErrPriceChanged, Errors: changed.Errors}
		}
		if err != nil {
			return err
		}
		if err := c.purchases.AddItems(ctx, p.ID, items); err != nil {
			return err
		}
		p.Items = items
		created = p
		return nil
	})
	if errors.Is(err, errKeyExists) {
		existing, err := c.purchases.ByKey(ctx, req.UserID, req.IdempotencyKey)
		if err != nil {
			return domain.Purchase{}, false, fmt.Errorf("ordering: read the purchase of an existing key: %w", err)
		}
		return existing, false, nil
	}
	if err != nil {
		return domain.Purchase{}, false, err
	}
	return created, true, nil
}

func rejection(req domain.CheckoutRequest, err error) error {
	var (
		unavailable *UnavailableProductError
		short       *InsufficientStockError
	)
	switch {
	case errors.As(err, &unavailable):
		return &RejectedError{Reason: ErrProductUnavailable, Errors: validation.Errors{{
			Field: domain.ItemField(indexOf(req, unavailable.ProductID), "product_id"), Code: CodeProductUnavailable,
			Message: "the product does not exist or was deleted",
		}}}
	case errors.As(err, &short):
		return &RejectedError{Reason: ErrInsufficientStock, Errors: validation.Errors{{
			Field: domain.ItemField(indexOf(req, short.ProductID), "quantity"), Code: CodeInsufficientStock,
			Message: fmt.Sprintf("only %d available", short.Available),
		}}}
	}
	return err
}

func indexOf(req domain.CheckoutRequest, id domain.ID) int {
	for _, it := range req.Items {
		if it.ProductID == id {
			return it.Index
		}
	}
	return 0
}

func (c *Checkout) replay(ctx context.Context, req domain.CheckoutRequest, existing domain.Purchase) (CheckoutResult, error) {
	switch {
	case existing.RequestHash != req.Hash:
		c.rec.Checkout(ctx, OutcomeIdempotencyConflict, nil, nil)
		return CheckoutResult{}, ErrIdempotencyConflict
	case !existing.Finished():
		c.rec.Checkout(ctx, OutcomeInProgress, &existing, nil)
		return CheckoutResult{Purchase: existing, Kind: InProgress}, nil
	}
	c.rec.Checkout(ctx, OutcomeReplayed, &existing, nil)
	return CheckoutResult{Purchase: existing, Kind: Replayed}, nil
}

func (c *Checkout) settle(ctx context.Context, p domain.Purchase) (domain.Purchase, error) {
	chargeCtx, cancelCharge := context.WithTimeout(context.WithoutCancel(ctx), c.budgets.Charge)
	outcome, err := c.charge(chargeCtx, c.payments, p)
	cancelCharge()
	if err != nil {
		return p, fmt.Errorf("ordering: charge: %w", err)
	}

	finalizeCtx, cancelFinalize := context.WithTimeout(context.WithoutCancel(ctx), c.budgets.Finalize)
	defer cancelFinalize()
	applied, err := c.finalize(finalizeCtx, p, outcome)
	if err != nil {
		return p, err
	}
	if !applied {
		current, err := c.purchases.Get(finalizeCtx, p.UserID, p.ID)
		if err != nil {
			return p, fmt.Errorf("ordering: read the purchase finalized elsewhere: %w", err)
		}
		return current, nil
	}
	if err := p.Apply(outcome); err != nil {
		return p, err
	}
	return p, nil
}

func reserveOutcome(err error) string {
	switch {
	case errors.Is(err, ErrInsufficientStock):
		return OutcomeInsufficientStock
	case errors.Is(err, ErrPriceChanged):
		return OutcomePriceChanged
	case errors.Is(err, ErrProductUnavailable):
		return OutcomeProductUnavailable
	case errors.Is(err, ErrBusy):
		return OutcomeTryAgain
	}
	return OutcomeError
}

func settledOutcome(p domain.Purchase) string {
	switch p.Status {
	case domain.StatusConfirmed:
		return OutcomeConfirmed
	case domain.StatusFailed:
		return OutcomeDeclined
	}
	return OutcomePending
}
