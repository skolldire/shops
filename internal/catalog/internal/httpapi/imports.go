package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/csvimport"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/httpx"
	"github.com/skolldire/shops/internal/platform/logger"
)

const importField = "file"

var importRetryAfter = strconv.Itoa(app.ImportRetryAfterSeconds)

type Importer interface {
	Import(ctx context.Context, r io.Reader, opts app.ImportOptions) (app.ImportReport, error)
}

type ImportHandler struct {
	importer Importer
	log      logger.Service
}

func NewImports(importer Importer, log logger.Service) (*ImportHandler, error) {
	switch {
	case importer == nil:
		return nil, errors.New("catalog: import handler requires an importer")
	case log == nil:
		return nil, errors.New("catalog: import handler requires a logger")
	}
	return &ImportHandler{importer: importer, log: log}, nil
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

type changeJSON struct {
	SKU         string `json:"sku"`
	StockBefore int    `json:"stock_before"`
	StockAfter  int    `json:"stock_after"`
	PriceBefore string `json:"price_before"`
	PriceAfter  string `json:"price_after"`
}

type importReportJSON struct {
	ImportID  string         `json:"import_id"`
	Mode      string         `json:"mode"`
	File      importFileJSON `json:"file"`
	Records   int            `json:"records"`
	Skipped   int            `json:"skipped"`
	Created   int            `json:"created"`
	Updated   int            `json:"updated"`
	Unchanged int            `json:"unchanged"`
	Rejected  int            `json:"rejected"`
	Errors    []rowErrorJSON `json:"errors"`
	Changes   []changeJSON   `json:"changes"`
}

func (h *ImportHandler) create(w http.ResponseWriter, r *http.Request) {
	opts, problem := importOptions(r)
	if problem != nil {
		httpx.WriteError(w, r, *problem)
		return
	}
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
		opts.FileName = part.FileName()
		report, err := h.importer.Import(r.Context(), part, opts)
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, reportJSON(report))
		return
	}
}

func importOptions(r *http.Request) (app.ImportOptions, *httpx.ErrorResponse) {
	query := r.URL.Query()
	mode, err := domain.ParseImportMode(query.Get("mode"))
	if err != nil {
		return app.ImportOptions{}, &httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_mode", Detail: "mode must be add or replace; it is replace when omitted."}
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if mode == domain.ImportAdd && key == "" {
		return app.ImportOptions{}, &httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_idempotency_key", Detail: "An add import requires an Idempotency-Key header with a UUID."}
	}
	allow := false
	switch raw := query.Get("allow_duplicate"); raw {
	case "", "false":
	case "true":
		allow = true
	default:
		return app.ImportOptions{}, &httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_request", Detail: "allow_duplicate must be true or false."}
	}
	return app.ImportOptions{Mode: mode, Key: key, AllowDuplicate: allow}, nil
}

func reportJSON(report app.ImportReport) importReportJSON {
	out := importReportJSON{
		ImportID:  report.ID,
		Mode:      string(report.Mode),
		Changes:   make([]changeJSON, len(report.Changes)),
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
	for i, c := range report.Changes {
		out.Changes[i] = changeJSON{
			SKU: c.SKU, StockBefore: c.StockBefore, StockAfter: c.StockAfter,
			PriceBefore: c.PriceBefore.StringFixed(2), PriceAfter: c.PriceAfter.StringFixed(2),
		}
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
	var (
		tooLarge  *http.MaxBytesError
		format    *csvimport.FormatError
		duplicate *app.DuplicateFileError
	)
	switch {
	case errors.Is(err, app.ErrInvalidIdempotencyKey), errors.Is(err, app.ErrIdempotencyKeyRequired):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_idempotency_key", Detail: "Idempotency-Key must be a UUID, and an add import requires one."})
	case errors.Is(err, app.ErrIdempotencyConflict):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusConflict, Code: "idempotency_conflict", Detail: "This Idempotency-Key was already used with another file or mode."})
	case errors.Is(err, app.ErrImportInProgress):
		w.Header().Set("Retry-After", importRetryAfter)
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusConflict, Code: "import_in_progress", Detail: "Another import is being applied. Nothing was imported; send the file again in a few seconds."})
	case errors.As(err, &duplicate):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusConflict, Code: "duplicate_file", Detail: fmt.Sprintf(
			"The same file was already added on %s. Send allow_duplicate=true to add it again.", duplicate.Previous.AppliedAt.UTC().Format(time.RFC3339))})
	case errors.Is(err, csvimport.ErrFileTooLarge), errors.As(err, &tooLarge):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusRequestEntityTooLarge, Code: "request_too_large", Detail: "The CSV file is larger than the import limit."})
	case errors.Is(err, csvimport.ErrInvalidHeader):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_header", Detail: "The first line must be exactly name,sku,description,category,price,stock,weight_kg."})
	case errors.As(err, &format):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "invalid_csv", Detail: fmt.Sprintf("Line %d: %s.", format.Line, format.Reason)})
	case errors.Is(err, csvimport.ErrTooManyRows):
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusBadRequest, Code: "too_many_rows", Detail: "The CSV has more rows than the import limit."})
	default:
		h.log.Error(r.Context(), err, map[string]any{"component": "catalog"})
		httpx.WriteError(w, r, httpx.ErrorResponse{Status: http.StatusInternalServerError, Code: "internal_error", Detail: "An unexpected error occurred."})
	}
}
