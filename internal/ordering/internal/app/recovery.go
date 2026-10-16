package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

const maxCauseLength = 500

var errAmountMismatch = errors.New("ordering: the provider charged another amount or currency")

type RecoveryPolicy struct {
	Owner         string
	MinAge        time.Duration
	Lease         time.Duration
	MaxPendingAge time.Duration
	Batch         int
	StuckAfter    int
	Backoff       func(attempts int) time.Duration
}

type StillPending struct {
	PurchaseID domain.ID
	Cause      error
}

type RecoveryReport struct {
	Confirmed    int
	Declined     int
	Voided       int
	Skipped      int
	StillPending []StillPending
}

type Recovery struct {
	finalizer
	payments Payments
	rec      CheckoutRecorder
	budgets  Budgets
	policy   RecoveryPolicy
}

func NewRecovery(tx Tx, stock Stock, payments Payments, purchases Purchases, carts Carts, rec CheckoutRecorder, b Budgets, policy RecoveryPolicy) (*Recovery, error) {
	switch {
	case tx == nil, stock == nil, payments == nil, purchases == nil, carts == nil, rec == nil:
		return nil, errors.New("ordering: recovery requires a transaction manager, stock, payments, purchases, carts and a recorder")
	case b.Charge <= 0, b.Finalize <= 0:
		return nil, errors.New("ordering: recovery time budgets must be positive")
	case policy.Owner == "", policy.Backoff == nil:
		return nil, errors.New("ordering: recovery requires an owner and a backoff")
	case policy.MinAge <= 0, policy.Lease <= 0, policy.MaxPendingAge <= 0, policy.Batch < 1, policy.StuckAfter < 1:
		return nil, errors.New("ordering: recovery requires a positive minimum age, lease, maximum pending age, batch and threshold")
	}
	return &Recovery{
		finalizer: finalizer{tx: tx, purchases: purchases, stock: stock, carts: carts},
		payments:  payments, rec: rec, budgets: b, policy: policy,
	}, nil
}

func (r *Recovery) RunCycle(ctx context.Context) (RecoveryReport, error) {
	claimed, err := r.purchases.Claim(ctx, r.policy.Owner, r.policy.MinAge, r.policy.Lease, r.policy.Batch)
	if err != nil {
		return RecoveryReport{}, fmt.Errorf("ordering: claim pending purchases: %w", err)
	}
	var report RecoveryReport
	for _, p := range claimed {
		if ctx.Err() != nil {
			break
		}
		r.recover(ctx, p, &report)
	}
	stats, err := r.purchases.PendingStats(context.WithoutCancel(ctx), r.policy.StuckAfter)
	if err != nil {
		return report, fmt.Errorf("ordering: read the pending purchases: %w", err)
	}
	r.rec.RecoveryGauges(ctx, stats)
	return report, nil
}

func (r *Recovery) recover(ctx context.Context, p domain.Purchase, report *RecoveryReport) {
	work := context.WithoutCancel(ctx)
	renewed, owned, err := r.purchases.Renew(work, p.ID, r.policy.Owner, r.policy.Lease, r.policy.MaxPendingAge)
	switch {
	case err != nil:
		report.StillPending = append(report.StillPending, StillPending{PurchaseID: p.ID, Cause: err})
		r.rec.RecoveryAttempt(ctx, RecoveryAttempt{PurchaseID: p.ID, Outcome: RecoveryError, Cause: err, StuckAfter: r.policy.StuckAfter})
		return
	case !owned:
		report.Skipped++
		r.rec.RecoveryAttempt(ctx, RecoveryAttempt{PurchaseID: p.ID, Outcome: RecoverySkipped, StuckAfter: r.policy.StuckAfter})
		return
	}
	if renewed.Attempts >= r.policy.StuckAfter || renewed.Overdue {
		r.resolve(work, p, renewed.Attempts, report)
		return
	}
	outcome, err := r.chargeWithin(work, p)
	if err != nil {
		r.reschedule(work, p, renewed.Attempts, err, report)
		return
	}
	r.settle(work, p, renewed.Attempts, outcome, report)
}

func (r *Recovery) chargeWithin(ctx context.Context, p domain.Purchase) (domain.Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, r.budgets.Charge)
	defer cancel()
	outcome, err := r.charge(ctx, r.payments, p)
	if err != nil {
		return domain.Outcome{}, fmt.Errorf("ordering: charge: %w", err)
	}
	return outcome, nil
}

func (r *Recovery) settle(ctx context.Context, p domain.Purchase, attempts int, o domain.Outcome, report *RecoveryReport) bool {
	finalizeCtx, cancel := context.WithTimeout(ctx, r.budgets.Finalize)
	defer cancel()
	applied, err := r.finalize(finalizeCtx, p, o)
	switch {
	case err != nil:
		r.reschedule(ctx, p, attempts, err, report)
		return false
	case !applied:
		report.Skipped++
		r.rec.RecoveryAttempt(ctx, RecoveryAttempt{PurchaseID: p.ID, Attempts: attempts, Outcome: RecoverySkipped, StuckAfter: r.policy.StuckAfter})
		return false
	case o.Status == domain.StatusConfirmed:
		report.Confirmed++
		r.rec.RecoveryAttempt(ctx, RecoveryAttempt{PurchaseID: p.ID, Attempts: attempts, Outcome: RecoveryConfirmed, StuckAfter: r.policy.StuckAfter})
	default:
		report.Declined++
		r.rec.RecoveryAttempt(ctx, RecoveryAttempt{PurchaseID: p.ID, Attempts: attempts, Outcome: RecoveryDeclined, StuckAfter: r.policy.StuckAfter})
	}
	return true
}

func (r *Recovery) reschedule(ctx context.Context, p domain.Purchase, attempts int, cause error, report *RecoveryReport) {
	report.StillPending = append(report.StillPending, StillPending{PurchaseID: p.ID, Cause: cause})
	outcome := attemptOutcome(cause)
	total, owned, err := r.purchases.Reschedule(ctx, p.ID, r.policy.Owner, r.policy.Backoff(attempts), sanitize(cause))
	switch {
	case err != nil:
		r.rec.RecoveryAttempt(ctx, RecoveryAttempt{PurchaseID: p.ID, Attempts: attempts, Outcome: RecoveryError, Cause: errors.Join(cause, err), StuckAfter: r.policy.StuckAfter})
	case !owned:
		r.rec.RecoveryAttempt(ctx, RecoveryAttempt{PurchaseID: p.ID, Attempts: attempts, Outcome: RecoverySkipped, Cause: cause, StuckAfter: r.policy.StuckAfter})
	default:
		r.rec.RecoveryAttempt(ctx, RecoveryAttempt{PurchaseID: p.ID, Attempts: total, Outcome: outcome, Cause: cause, StuckAfter: r.policy.StuckAfter})
	}
}

func (r *Recovery) resolve(ctx context.Context, p domain.Purchase, attempts int, report *RecoveryReport) {
	lookupCtx, cancel := context.WithTimeout(ctx, r.budgets.Charge)
	found, err := r.payments.Lookup(lookupCtx, string(p.ID))
	cancel()
	if err == nil && found.Status == PaymentNotFound {
		voidCtx, cancel := context.WithTimeout(ctx, r.budgets.Charge)
		found, err = r.payments.Void(voidCtx, string(p.ID))
		cancel()
	}
	if err != nil {
		r.resolution(ctx, p, attempts, ResolutionError, err)
		r.reschedule(ctx, p, attempts, err, report)
		return
	}
	var (
		outcome domain.Outcome
		result  string
	)
	switch {
	case found.Status == PaymentApproved && (!found.Amount.Equal(p.Total) || found.Currency != p.Currency):
		cause := fmt.Errorf("%w: %s %s instead of %s %s", errAmountMismatch,
			found.Amount.StringFixed(2), found.Currency, p.Total.StringFixed(2), p.Currency)
		r.resolution(ctx, p, attempts, ResolutionMismatch, cause)
		r.reschedule(ctx, p, attempts, cause, report)
		return
	case found.Status == PaymentApproved:
		outcome, result = domain.Confirmed(found.ProviderRef), ResolutionConfirmed
	case found.Status == PaymentDeclined:
		outcome, result = domain.Failed(found.Reason), ResolutionDeclined
	case found.Status == PaymentVoided:
		outcome, result = domain.Failed(ReasonPaymentNotCompleted), ResolutionVoided
	default:
		cause := fmt.Errorf("%w: the provider answered %q", ErrPaymentUnavailable, found.Status)
		r.resolution(ctx, p, attempts, ResolutionError, cause)
		r.reschedule(ctx, p, attempts, cause, report)
		return
	}
	if r.settle(ctx, p, attempts, outcome, report) {
		if result == ResolutionVoided {
			report.Voided++
		}
		r.resolution(ctx, p, attempts, result, nil)
	}
}

func (r *Recovery) resolution(ctx context.Context, p domain.Purchase, attempts int, result string, cause error) {
	r.rec.RecoveryResolution(ctx, RecoveryResolution{PurchaseID: p.ID, Attempts: attempts, Result: result, Cause: cause})
}

func attemptOutcome(cause error) string {
	switch {
	case errors.Is(cause, ErrPaymentConflict), errors.Is(cause, errAmountMismatch):
		return RecoveryError
	case errors.Is(cause, ErrPaymentOutcomeUnknown), errors.Is(cause, ErrPaymentUnavailable):
		return RecoveryStillPending
	}
	return RecoveryError
}

func sanitize(err error) string {
	text := err.Error()
	if len(text) > maxCauseLength {
		text = text[:maxCauseLength]
	}
	return text
}
