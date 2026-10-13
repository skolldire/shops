package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
)

const importField = "file"

type Importer interface {
	Import(ctx context.Context, r io.Reader, fileName string) (app.ImportReport, error)
}

type ImportRejection int

const (
	NotRejected ImportRejection = iota
	RejectedFileTooLarge
	RejectedInvalidHeader
	RejectedInvalidFormat
	RejectedTooManyRows
)

type ImportFailure struct {
	Rejection ImportRejection
	Line      int
	Reason    string
}

type ImportFailureClassifier func(err error) ImportFailure

type ImportHandler struct {
	importer Importer
	classify ImportFailureClassifier
	log      logger.Service
}

func NewImports(importer Importer, classify ImportFailureClassifier, log logger.Service) (*ImportHandler, error) {
	switch {
	case importer == nil:
		return nil, errors.New("catalog: import handler requires an importer")
	case classify == nil:
		return nil, errors.New("catalog: import handler requires a failure classifier")
	case log == nil:
		return nil, errors.New("catalog: import handler requires a logger")
	}
	return &ImportHandler{importer: importer, classify: classify, log: log}, nil
}

func (h *ImportHandler) Routes(r chi.Router) {
	r.Post("/imports", h.create)
}

type importFileJSON struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type rowErrorJSON struct {
	Line    int    `json:"line"`
	SKU     string `json:"sku"`
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type importReportJSON struct {
	File      importFileJSON `json:"file"`
	Records   int            `json:"records"`
	Skipped   int            `json:"skipped"`
	Created   int            `json:"created"`
	Updated   int            `json:"updated"`
	Unchanged int            `json:"unchanged"`
	Rejected  int            `json:"rejected"`
	Errors    []rowErrorJSON `json:"errors"`
}

func (h *ImportHandler) create(w http.ResponseWriter, r *http.Request) {
	parts, err := r.MultipartReader()
	if err != nil {
		h.invalidRequest(w, r)
		return
	}
	for {
		part, err := parts.NextPart()
		if err != nil {
			h.partError(w, r, err)
			return
		}
		if part.FormName() != importField {
			continue
		}
		report, err := h.importer.Import(r.Context(), part, part.FileName())
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, reportJSON(report))
		return
	}
}

func reportJSON(report app.ImportReport) importReportJSON {
	out := importReportJSON{
		File:      importFileJSON{Name: report.File.Name, Bytes: report.File.Bytes, SHA256: report.File.SHA256},
		Records:   report.Records,
		Skipped:   report.Skipped,
		Created:   report.Created,
		Updated:   report.Updated,
		Unchanged: report.Unchanged,
		Rejected:  report.Rejected,
		Errors:    make([]rowErrorJSON, len(report.Errors)),
	}
	for i, e := range report.Errors {
		out.Errors[i] = rowErrorJSON{Line: e.Line, SKU: e.SKU, Field: e.Field, Code: e.Code, Message: e.Message}
	}
	return out
}

func (h *ImportHandler) invalidRequest(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, httpx.ErrorResponse{
		Status: http.StatusBadRequest, Code: "invalid_request",
		Detail: "The request must be multipart/form-data with the CSV in a field named file.",
	})
}

func (h *ImportHandler) partError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		h.writeError(w, r, err)
		return
	}
	h.invalidRequest(w, r)
}

func (h *ImportHandler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	failure := h.classify(err)
	switch {
	case failure.Rejection == RejectedFileTooLarge, errors.As(err, &tooLarge):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusRequestEntityTooLarge, Code: "request_too_large", Detail: "The CSV file is larger than the import limit."})
	case failure.Rejection == RejectedInvalidHeader:
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_header", Detail: "The first line must be exactly name,sku,description,category,price,stock,weight_kg."})
	case failure.Rejection == RejectedInvalidFormat:
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_csv", Detail: fmt.Sprintf("Line %d: %s.", failure.Line, failure.Reason)})
	case failure.Rejection == RejectedTooManyRows:
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "too_many_rows", Detail: "The CSV has more rows than the import limit."})
	default:
		h.log.Error(r.Context(), err, map[string]any{"component": "catalog"})
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusInternalServerError, Code: "internal_error", Detail: "An unexpected error occurred."})
	}
}
