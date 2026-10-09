package httpapi

import (
	"errors"
	"net/http"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/validation"
)

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var fieldErrs validation.Errors
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
	case errors.Is(err, core.ErrNotFound):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusNotFound, Code: "not_found", Detail: "The product does not exist."})
	case errors.Is(err, core.ErrSKUTaken):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusConflict, Code: "sku_taken", Detail: "Another product already uses this SKU."})
	case errors.Is(err, core.ErrVersionConflict):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusPreconditionFailed, Code: "version_conflict", Detail: "The product changed since it was read. Fetch it again and retry."})
	default:
		h.log.Error(r.Context(), err, map[string]any{"component": "catalog"})
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusInternalServerError, Code: "internal_error", Detail: "An unexpected error occurred."})
	}
}
