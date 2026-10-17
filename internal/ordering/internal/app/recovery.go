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
	MinLockTimeout = time.Millisecond
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

type RecoveryPorts struct {
	Settlement
	Pending  PendingPurchases
	Payments Payments
	Recorder RecoveryRecorder
}

type Recovery struct {
	finalizer
	pending  PendingPurchases
	payments Payments
	rec      RecoveryRecorder
	budgets  Budgets
	policy   RecoveryPolicy
	item     time.Duration
}

func NewRecovery(p RecoveryPorts, b Budgets, policy RecoveryPolicy) (*Recovery, error) {
	switch {
	case !p.complete(), p.Pending == nil, p.Payments == nil, p.Recorder == nil:
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
		finalizer: p.finalizer(), pending: p.Pending,
		payments: p.Payments, rec: p.Recorder, budgets: b, policy: policy, item: ItemBudget(b, policy.DBTimeout),
	}, nil
}

func (r *Recovery) RunCycle(ctx context.Context) (RecoveryReport, error) {
	claimCtx, cancel := context.WithTimeout(ctx, r.policy.DBTimeout)
	claimed, err := r.pending.Claim(claimCtx, r.policy.Owner, r.policy.MinAge, r.policy.Lease, r.policy.Batch)
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
	stats, err := r.pending.PendingStats(statsCtx, r.policy.StuckAfter)
	if err != nil {
		return report, fmt.Errorf("ordering: the pending purchase gauges were not updated: %w", err)
	}
	r.rec.RecoveryGauges(ctx, stats)
	return report, nil
}

type recoveryResult struct {
	outcome  string
	attempts int
	cause    error
	voided   bool
}

func (r *Recovery) record(ctx context.Context, p domain.Purchase, res recoveryResult, report *RecoveryReport) {
	switch res.outcome {
	case RecoveryConfirmed:
		report.Confirmed++
	case RecoveryDeclined:
		report.Declined++
		if res.voided {
			report.Voided++
		}
	case RecoverySkipped:
		report.Skipped++
	default:
		report.StillPending = append(report.StillPending, StillPending{PurchaseID: p.ID, Cause: res.cause})
	}
	r.rec.RecoveryAttempt(ctx, RecoveryAttempt{PurchaseID: p.ID, Attempts: res.attempts, Outcome: res.outcome, Cause: res.cause, StuckAfter: r.policy.StuckAfter})
}

func (r *Recovery) recover(ctx context.Context, p domain.Purchase, report *RecoveryReport) {
	start := time.Now()
	item, cancelItem := context.WithDeadline(context.WithoutCancel(ctx), start.Add(r.item))
	defer cancelItem()
	renewCtx, cancel := context.WithDeadline(ctx, start.Add(r.policy.DBTimeout))
	renewed, owned, err := r.pending.Renew(renewCtx, p.ID, r.policy.Owner, r.policy.Lease, r.policy.MaxPendingAge)
	cancel()
	switch {
	case err != nil && ctx.Err() != nil:
		return
	case err != nil:
		r.record(ctx, p, recoveryResult{outcome: RecoveryError, cause: err}, report)
		return
	case !owned:
		r.record(ctx, p, recoveryResult{outcome: RecoverySkipped}, report)
		return
	}
	if renewed.Attempts >= r.policy.StuckAfter || renewed.Overdue {
		r.record(item, p, r.resolve(item, p, renewed.Attempts), report)
		return
	}
	outcome, err := r.chargeWithin(item, p)
	if err != nil {
		r.record(item, p, r.reschedule(item, p, renewed.Attempts, err), report)
		return
	}
	r.record(item, p, r.settle(item, p, renewed.Attempts, outcome), report)
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

func (r *Recovery) settle(ctx context.Context, p domain.Purchase, attempts int, o domain.Outcome) recoveryResult {
	applied, err := r.finalizeWithin(ctx, p, o)
	switch {
	case err != nil:
		return r.reschedule(ctx, p, attempts, err)
	case !applied:
		return recoveryResult{outcome: RecoverySkipped, attempts: attempts}
	case o.Status == domain.StatusConfirmed:
		return recoveryResult{outcome: RecoveryConfirmed, attempts: attempts}
	}
	return recoveryResult{outcome: RecoveryDeclined, attempts: attempts}
}

func (r *Recovery) finalizeWithin(item context.Context, p domain.Purchase, o domain.Outcome) (bool, error) {
	ctx, cancel, err := within(item, r.budgets.Finalize)
	if err != nil {
		return false, err
	}
	defer cancel()
	return r.finalize(ctx, p, o)
}

func (r *Recovery) reschedule(ctx context.Context, p domain.Purchase, attempts int, cause error) recoveryResult {
	var (
		total int
		owned bool
	)
	rescheduleCtx, cancel, err := within(ctx, r.policy.DBTimeout)
	if err == nil {
		total, owned, err = r.pending.Reschedule(rescheduleCtx, p.ID, r.policy.Owner, r.policy.Backoff(attempts), sanitize(cause))
		cancel()
	}
	switch {
	case err != nil:
		return recoveryResult{outcome: RecoveryError, attempts: attempts, cause: errors.Join(cause, err)}
	case !owned:
		return recoveryResult{outcome: RecoverySkipped, attempts: attempts, cause: cause}
	}
	return recoveryResult{outcome: attemptOutcome(cause), attempts: total, cause: cause}
}

func (r *Recovery) resolve(ctx context.Context, p domain.Purchase, attempts int) recoveryResult {
	found, err := r.ask(ctx, r.payments.Lookup, string(p.ID))
	if err == nil && found.Status == PaymentNotFound {
		found, err = r.ask(ctx, r.payments.Void, string(p.ID))
	}
	if err != nil {
		r.resolution(ctx, p, attempts, ResolutionError, err)
		return r.reschedule(ctx, p, attempts, err)
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
		return r.reschedule(ctx, p, attempts, cause)
	case found.Status == PaymentApproved:
		outcome, result = domain.Confirmed(found.ProviderRef), ResolutionConfirmed
	case found.Status == PaymentDeclined:
		outcome, result = domain.Failed(found.Reason), ResolutionDeclined
	case found.Status == PaymentVoided:
		outcome, result = domain.Failed(ReasonPaymentNotCompleted), ResolutionVoided
	default:
		cause := fmt.Errorf("%w: the provider answered %q", ErrPaymentUnavailable, found.Status)
		r.resolution(ctx, p, attempts, ResolutionError, cause)
		return r.reschedule(ctx, p, attempts, cause)
	}
	res := r.settle(ctx, p, attempts, outcome)
	if res.outcome == RecoveryConfirmed || res.outcome == RecoveryDeclined {
		res.voided = result == ResolutionVoided
		r.resolution(ctx, p, attempts, result, nil)
	}
	return res
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
