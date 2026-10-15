package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type finalizer struct {
	tx        Tx
	purchases Purchases
	stock     Stock
	carts     Carts
}

func (f finalizer) finalize(ctx context.Context, p domain.Purchase, o domain.Outcome) (bool, error) {
	applied := false
	err := f.tx.WithinTx(ctx, func(ctx context.Context) error {
		applied = false
		won, err := f.purchases.Finish(ctx, p.ID, o)
		if err != nil || !won {
			return err
		}
		switch o.Status {
		case domain.StatusConfirmed:
			if err = f.stock.Commit(ctx, p.Quantities(), string(p.ID)); err == nil {
				err = f.carts.RemovePurchased(ctx, p.UserID, p.Items)
			}
		case domain.StatusFailed:
			err = f.stock.Release(ctx, p.Quantities(), string(p.ID))
		default:
			err = domain.ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("ordering: finalize purchase: %w", err)
	}
	return applied, nil
}

func (f finalizer) charge(ctx context.Context, payments Payments, p domain.Purchase) (domain.Outcome, error) {
	res, err := payments.Charge(ctx, Charge{Reference: string(p.ID), Token: p.PaymentToken, Amount: p.Total, Currency: p.Currency})
	if err != nil {
		return domain.Outcome{}, err
	}
	if res.Approved {
		return domain.Confirmed(res.ProviderRef), nil
	}
	if res.Reason == "" {
		return domain.Outcome{}, errors.New("ordering: the payment was declined without a reason")
	}
	return domain.Failed(res.Reason), nil
}
