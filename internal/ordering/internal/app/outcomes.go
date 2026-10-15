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
)
