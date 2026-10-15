package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

type StillPending struct {
	PurchaseID domain.ID
	Cause      error
}

type RecoveryReport struct {
	Confirmed    int
	Declined     int
	StillPending []StillPending
}

type Recovery struct {
	finalizer
	payments Payments
	rec      CheckoutRecorder
	budgets  Budgets
	total    time.Duration
}

func NewRecovery(tx Tx, stock Stock, payments Payments, purchases Purchases, carts Carts, rec CheckoutRecorder, b Budgets, total time.Duration) (*Recovery, error) {
	switch {
	case tx == nil, stock == nil, payments == nil, purchases == nil, carts == nil, rec == nil:
		return nil, errors.New("ordering: recovery requires a transaction manager, stock, payments, purchases, carts and a recorder")
	case b.Charge <= 0, b.Finalize <= 0, total <= 0:
		return nil, errors.New("ordering: recovery time budgets must be positive")
	}
	return &Recovery{
		finalizer: finalizer{tx: tx, purchases: purchases, stock: stock, carts: carts},
		payments:  payments, rec: rec, budgets: b, total: total,
	}, nil
}

func (r *Recovery) RecoverPending(ctx context.Context) (RecoveryReport, error) {
	ctx, cancel := context.WithTimeout(ctx, r.total)
	defer cancel()

	pending, err := r.purchases.Pending(ctx)
	if err != nil {
		return RecoveryReport{}, fmt.Errorf("ordering: list pending purchases: %w", err)
	}
	var report RecoveryReport
	for _, p := range pending {
		status, err := r.recover(ctx, p)
		switch {
		case err != nil:
			report.StillPending = append(report.StillPending, StillPending{PurchaseID: p.ID, Cause: err})
			r.rec.Recovery(ctx, RecoveryStillPending)
		case status == domain.StatusConfirmed:
			report.Confirmed++
			r.rec.Recovery(ctx, RecoveryConfirmed)
		default:
			report.Declined++
			r.rec.Recovery(ctx, RecoveryDeclined)
		}
	}
	return report, nil
}

func (r *Recovery) recover(ctx context.Context, p domain.Purchase) (domain.Status, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("ordering: recovery time budget exhausted: %w", err)
	}
	chargeCtx, cancelCharge := context.WithTimeout(ctx, r.budgets.Charge)
	outcome, err := r.charge(chargeCtx, r.payments, p)
	cancelCharge()
	if err != nil {
		return "", fmt.Errorf("ordering: charge: %w", err)
	}
	finalizeCtx, cancelFinalize := context.WithTimeout(ctx, r.budgets.Finalize)
	defer cancelFinalize()
	if _, err := r.finalize(finalizeCtx, p, outcome); err != nil {
		return "", err
	}
	return outcome.Status, nil
}
