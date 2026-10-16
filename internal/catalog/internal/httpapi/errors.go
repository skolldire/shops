package httpapi

import (
	"errors"
	"net/http"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/validation"
)

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		fieldErrs validation.Errors
		below     *app.BelowReservedError
	)
	switch {
	case errors.As(err, &fieldErrs):
		httpx.WriteError(w, r, httpx.ErrorResponse{
			Status: http.StatusUnprocessableEntity,
			Code:   "validation_failed",
			Detail: "One or more fields are invalid.",
			Errors: fieldErrs,
		})
	case errors.Is(err, errInvalidRequest), errors.Is(err, errInvalidPrecondition):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_request", Detail: err.Error()})
	case errors.Is(err, errRequestTooLarge):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusRequestEntityTooLarge, Code: "request_too_large", Detail: "The request body must be at most 1 MB."})
	case errors.Is(err, errPreconditionRequired):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusPreconditionRequired, Code: "precondition_required", Detail: err.Error()})
	case errors.Is(err, errWeakPrecondition):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusPreconditionFailed, Code: "precondition_failed", Detail: err.Error()})
	case errors.Is(err, app.ErrNotFound):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusNotFound, Code: "not_found", Detail: "The product does not exist."})
	case errors.Is(err, app.ErrSKUTaken):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusConflict, Code: "sku_taken", Detail: "Another product already uses this SKU."})
	case errors.Is(err, app.ErrImportInProgress):
		w.Header().Set("Retry-After", importRetryAfter)
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusConflict, Code: "import_in_progress", Detail: "An import is being applied. Nothing was created; send the product again in a few seconds."})
	case errors.As(err, &below):
		httpx.WriteError(w, r, httpx.ErrorResponse{
			Status: http.StatusConflict,
			Code:   app.CodeBelowReserved,
			Detail: "The stock cannot go below the units reserved by pending purchases.",
			Errors: validation.Errors{{Field: "stock", Code: app.CodeBelowReserved, Message: below.Message()}},
		})
	case errors.Is(err, app.ErrVersionConflict):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusPreconditionFailed, Code: "version_conflict", Detail: "The product changed since it was read. Fetch it again and retry."})
	default:
		h.log.Error(r.Context(), err, map[string]any{"component": "catalog"})
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusInternalServerError, Code: "internal_error", Detail: "An unexpected error occurred."})
	}
}
