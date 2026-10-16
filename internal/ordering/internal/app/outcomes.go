package app

const (
	OutcomeConfirmed           = "confirmed"
	OutcomeDeclined            = "declined"
	OutcomePending             = "pending"
	OutcomeInsufficientStock   = "insufficient_stock"
	OutcomePriceChanged        = "price_changed"
	OutcomeProductUnavailable  = "product_unavailable"
	OutcomeInvalid             = "invalid"
	OutcomeIdempotencyConflict = "idempotency_conflict"
	OutcomeReplayed            = "replayed"
	OutcomeInProgress          = "in_progress"
	OutcomeTryAgain            = "try_again"
	OutcomeError               = "error"

	RecoveryConfirmed    = "confirmed"
	RecoveryDeclined     = "declined"
	RecoveryStillPending = "still_pending"
	RecoveryError        = "error"
	RecoverySkipped      = "skipped"

	ResolutionConfirmed = "confirmed"
	ResolutionDeclined  = "declined"
	ResolutionVoided    = "voided"
	ResolutionMismatch  = "mismatch"
	ResolutionError     = "error"

	ReasonPaymentNotCompleted = "payment_not_completed"
)
