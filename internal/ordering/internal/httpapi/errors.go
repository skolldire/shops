package httpapi

import (
	"errors"
	"net/http"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/validation"
)

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		fieldErrs validation.Errors
		rejected  *app.RejectedError
	)
	switch {
	case errors.Is(err, errInvalidRequest):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_request", Detail: err.Error()})
	case errors.Is(err, errRequestTooLarge):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusRequestEntityTooLarge, Code: "request_too_large", Detail: "The request body must be at most 1 MB."})
	case errors.Is(err, domain.ErrInvalidIdempotencyKey):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_idempotency_key",
			Detail: "The " + IdempotencyKeyHeader + " header is required and must be a UUID such as 2b6f0c3e-8d4a-4f6b-9e2a-1c0d5e7f9a3b."})
	case errors.As(err, &fieldErrs):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Detail: "One or more fields are invalid.", Errors: fieldErrs})
	case errors.As(err, &rejected):
		h.writeRejection(w, r, rejected)
	case errors.Is(err, app.ErrIdempotencyConflict):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusConflict, Code: "idempotency_conflict",
			Detail: "This idempotency key was already used with a different request. Use a new key for a new purchase."})
	case errors.Is(err, app.ErrBusy):
		w.Header().Set("Retry-After", "1")
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusServiceUnavailable, Code: "try_again",
			Detail: "The purchase could not be reserved because of a concurrent operation and nothing was applied. Retry with the same idempotency key."})
	case errors.Is(err, app.ErrPurchaseNotFound):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusNotFound, Code: "not_found", Detail: "The purchase does not exist."})
	default:
		h.log.Error(r.Context(), err, map[string]any{"component": "ordering"})
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusInternalServerError, Code: "internal_error", Detail: "An unexpected error occurred."})
	}
}

func (h *Handler) writeRejection(w http.ResponseWriter, r *http.Request, rejected *app.RejectedError) {
	res := httpx.ErrorResponse{Errors: rejected.Errors}
	switch {
	case errors.Is(rejected, app.ErrInsufficientStock):
		res.Status, res.Code, res.Detail = http.StatusConflict, "insufficient_stock", "There is not enough stock for one of the items."
	case errors.Is(rejected, app.ErrPriceChanged):
		res.Status, res.Code, res.Detail = http.StatusConflict, "price_changed", "The price of one or more items changed. Review the current prices and retry."
	default:
		res.Status, res.Code, res.Detail = http.StatusUnprocessableEntity, "product_unavailable", "One of the products does not exist or was deleted."
	}
	httpx.WriteError(w, r, res)
}
