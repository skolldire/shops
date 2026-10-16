package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
)

const (
	maxCauseLength = 500
	LeaseMargin    = 2 * time.Second
)

var (
	errAmountMismatch = errors.New("ordering: the provider charged another amount or currency")
	errDeadlineSpent  = errors.New("ordering: the purchase used up its recovery deadline, so its lease expires on its own")
)

func ItemBudget(b Budgets, dbTimeout time.Duration) time.Duration {
	renew, reschedule := dbTimeout, dbTimeout
	lookup, void := b.Charge, b.Charge
	return renew + max(b.Charge, lookup+void) + b.Finalize + reschedule
}

type RecoveryPolicy struct {
	Owner         string
	MinAge        time.Duration
	Lease         time.Duration
	MaxPendingAge time.Duration
	DBTimeout     time.Duration
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
	purchases RecoveryPurchases
	payments  Payments
	rec       CheckoutRecorder
	budgets   Budgets
	policy    RecoveryPolicy
	item      time.Duration
}

func NewRecovery(tx Tx, stock StockSettlement, payments Payments, purchases RecoveryPurchases, carts PurchasedCartRemover, rec CheckoutRecorder, b Budgets, policy RecoveryPolicy) (*Recovery, error) {
	switch {
	case tx == nil, stock == nil, payments == nil, purchases == nil, carts == nil, rec == nil:
		return nil, errors.New("ordering: recovery requires a transaction manager, stock, payments, purchases, carts and a recorder")
	case b.Charge <= 0, b.Finalize <= 0:
		return nil, errors.New("ordering: recovery time budgets must be positive")
	case policy.Owner == "", policy.Backoff == nil:
		return nil, errors.New("ordering: recovery requires an owner and a backoff")
	case policy.MinAge <= 0, policy.Lease <= 0, policy.MaxPendingAge <= 0, policy.DBTimeout <= 0, policy.Batch < 1, policy.StuckAfter < 1:
		return nil, errors.New("ordering: recovery requires a positive minimum age, lease, maximum pending age, database timeout, batch and threshold")
	case policy.Lease <= ItemBudget(b, policy.DBTimeout)+LeaseMargin:
		return nil, errors.New("ordering: the recovery lease must exceed the deadline of one purchase plus the margin")
	}
	return &Recovery{
		finalizer: finalizer{tx: tx, purchases: purchases, stock: stock, carts: carts},
		purchases: purchases,
		payments:  payments, rec: rec, budgets: b, policy: policy, item: ItemBudget(b, policy.DBTimeout),
	}, nil
}

func (r *Recovery) RunCycle(ctx context.Context) (RecoveryReport, error) {
	claimCtx, cancel := context.WithTimeout(ctx, r.policy.DBTimeout)
	claimed, err := r.purchases.Claim(claimCtx, r.policy.Owner, r.policy.MinAge, r.policy.Lease, r.policy.Batch)
	cancel()
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
	if ctx.Err() != nil {
		return report, nil
	}
	statsCtx, cancel := context.WithTimeout(ctx, r.policy.DBTimeout)
	defer cancel()
	stats, err := r.purchases.PendingStats(statsCtx, r.policy.StuckAfter)
	if err != nil {
		return report, fmt.Errorf("ordering: the pending purchase gauges were not updated: %w", err)
	}
	r.rec.RecoveryGauges(ctx, stats)
	return report, nil
}

func (r *Recovery) recover(ctx context.Context, p domain.Purchase, report *RecoveryReport) {
	start := time.Now()
	item, cancelItem := context.WithDeadline(context.WithoutCancel(ctx), start.Add(r.item))
	defer cancelItem()
	renewCtx, cancel := context.WithDeadline(ctx, start.Add(r.policy.DBTimeout))
	renewed, owned, err := r.purchases.Renew(renewCtx, p.ID, r.policy.Owner, r.policy.Lease, r.policy.MaxPendingAge)
	cancel()
	switch {
	case err != nil && ctx.Err() != nil:
		return
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
		r.resolve(item, p, renewed.Attempts, report)
		return
	}
	outcome, err := r.chargeWithin(item, p)
	if err != nil {
		r.reschedule(item, p, renewed.Attempts, err, report)
		return
	}
	r.settle(item, p, renewed.Attempts, outcome, report)
}

func within(item context.Context, budget time.Duration) (context.Context, context.CancelFunc, error) {
	if err := item.Err(); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errDeadlineSpent, err)
	}
	ctx, cancel := context.WithTimeout(item, budget)
	return ctx, cancel, nil
}

func (r *Recovery) chargeWithin(item context.Context, p domain.Purchase) (domain.Outcome, error) {
	ctx, cancel, err := within(item, r.budgets.Charge)
	if err != nil {
		return domain.Outcome{}, err
	}
	defer cancel()
	outcome, err := r.charge(ctx, r.payments, p)
	if err != nil {
		return domain.Outcome{}, fmt.Errorf("ordering: charge: %w", err)
	}
	return outcome, nil
}

func (r *Recovery) settle(ctx context.Context, p domain.Purchase, attempts int, o domain.Outcome, report *RecoveryReport) bool {
	applied, err := r.finalizeWithin(ctx, p, o)
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

func (r *Recovery) finalizeWithin(item context.Context, p domain.Purchase, o domain.Outcome) (bool, error) {
	ctx, cancel, err := within(item, r.budgets.Finalize)
	if err != nil {
		return false, err
	}
	defer cancel()
	return r.finalize(ctx, p, o)
}

func (r *Recovery) reschedule(ctx context.Context, p domain.Purchase, attempts int, cause error, report *RecoveryReport) {
	report.StillPending = append(report.StillPending, StillPending{PurchaseID: p.ID, Cause: cause})
	outcome := attemptOutcome(cause)
	var (
		total int
		owned bool
	)
	rescheduleCtx, cancel, err := within(ctx, r.policy.DBTimeout)
	if err == nil {
		total, owned, err = r.purchases.Reschedule(rescheduleCtx, p.ID, r.policy.Owner, r.policy.Backoff(attempts), sanitize(cause))
		cancel()
	}
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
	found, err := r.ask(ctx, r.payments.Lookup, string(p.ID))
	if err == nil && found.Status == PaymentNotFound {
		found, err = r.ask(ctx, r.payments.Void, string(p.ID))
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

func (r *Recovery) ask(item context.Context, call func(context.Context, string) (PaymentRecord, error), reference string) (PaymentRecord, error) {
	ctx, cancel, err := within(item, r.budgets.Charge)
	if err != nil {
		return PaymentRecord{}, err
	}
	defer cancel()
	return call(ctx, reference)
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
