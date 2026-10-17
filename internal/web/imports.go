package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/skolldire/shops/internal/catalog"
)

var importRetryAfter = strconv.Itoa(catalog.ImportRetryAfterSeconds)

var (
	errInvalidAllowDuplicate = errors.New("web: allow_duplicate must be true or false")
	errImportFieldTooLong    = errors.New("web: an import form field is longer than allowed")
)

const (
	adminImports         = "/admin/imports"
	importField          = "file"
	maxShownImportErrors = 200
	maxShownChanges      = 200
	maxImportFieldBytes  = 64
)

type importPage struct {
	Problem     string
	Key         string
	Mode        catalog.ImportMode
	DuplicateOf *time.Time
	Report      *catalog.ImportReport
	Errors      []catalog.RowError
	More        int
	Changes     []catalog.ImportChange
	MoreChanges int
}

func (ui *UI) importForm(w http.ResponseWriter, r *http.Request) {
	ui.page(w, r, http.StatusOK, "admin_import", importPage{Key: uuid.NewString()})
}

func (ui *UI) importFile(w http.ResponseWriter, r *http.Request) {
	parts, err := r.MultipartReader()
	if err != nil {
		ui.importProblem(w, r, http.StatusBadRequest, "Choose a CSV file to import.")
		return
	}
	var opts catalog.ImportOptions
	for {
		part, err := parts.NextPart()
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			ui.importFailed(w, r, opts, err)
			return
		case err != nil:
			ui.importProblem(w, r, http.StatusBadRequest, "Choose a CSV file to import.")
			return
		}
		if part.FormName() != importField {
			if err := readImportField(part.FormName(), part, &opts); err != nil {
				ui.importFailed(w, r, opts, err)
				return
			}
			continue
		}
		if opts.Mode != catalog.ImportAdd && opts.Mode != catalog.ImportReplace {
			ui.importProblem(w, r, http.StatusBadRequest, "Choose whether to add to or replace the inventory before the file.")
			return
		}
		opts.FileName = part.FileName()
		report, err := ui.catalog.Import(r.Context(), part, opts)
		if err != nil {
			ui.importFailed(w, r, opts, err)
			return
		}
		ui.page(w, r, http.StatusOK, "admin_import", reportPage(report))
		return
	}
}

func readImportField(name string, r io.Reader, opts *catalog.ImportOptions) error {
	value, err := io.ReadAll(io.LimitReader(r, maxImportFieldBytes+1))
	if err != nil {
		return err
	}
	if len(value) > maxImportFieldBytes {
		return errImportFieldTooLong
	}
	text := strings.TrimSpace(string(value))
	switch name {
	case "mode":
		opts.Mode = catalog.ImportMode(text)
	case "idempotency_key":
		opts.IdempotencyKey = text
	case "allow_duplicate":
		switch text {
		case "true":
			opts.AllowDuplicate = true
		case "false":
			opts.AllowDuplicate = false
		default:
			return errInvalidAllowDuplicate
		}
	}
	return nil
}

func reportPage(report catalog.ImportReport) importPage {
	data := importPage{Key: uuid.NewString(), Report: &report, Errors: report.Errors, Changes: report.Changes}
	if len(data.Errors) > maxShownImportErrors {
		data.Errors, data.More = data.Errors[:maxShownImportErrors], len(report.Errors)-maxShownImportErrors
	}
	if len(data.Changes) > maxShownChanges {
		data.Changes, data.MoreChanges = data.Changes[:maxShownChanges], len(report.Changes)-maxShownChanges
	}
	return data
}

func (ui *UI) importFailed(w http.ResponseWriter, r *http.Request, opts catalog.ImportOptions, err error) {
	var (
		tooLarge  *http.MaxBytesError
		invalid   *catalog.InvalidCSVError
		duplicate *catalog.DuplicateFileError
	)
	switch {
	case errors.As(err, &duplicate):
		at := duplicate.AppliedAt
		ui.page(w, r, http.StatusConflict, "admin_import", importPage{Key: opts.IdempotencyKey, Mode: catalog.ImportAdd, DuplicateOf: &at})
	case errors.Is(err, catalog.ErrFileTooLarge), errors.As(err, &tooLarge):
		ui.importProblem(w, r, http.StatusRequestEntityTooLarge, "The file is larger than the import limit. Nothing was imported.")
	case errors.Is(err, catalog.ErrInvalidHeader):
		ui.importProblem(w, r, http.StatusBadRequest, "The first line must be exactly name,sku,description,category,price,stock,weight_kg. Nothing was imported.")
	case errors.As(err, &invalid):
		ui.importProblem(w, r, http.StatusBadRequest, fmt.Sprintf("The file is not a valid UTF-8 CSV. Line %d: %s. Nothing was imported.", invalid.Line, invalid.Reason))
	case errors.Is(err, catalog.ErrTooManyRows):
		ui.importProblem(w, r, http.StatusBadRequest, "The file has more rows than the import limit. Nothing was imported.")
	case errors.Is(err, catalog.ErrImportInProgress):
		w.Header().Set("Retry-After", importRetryAfter)
		ui.importProblem(w, r, http.StatusConflict, "Another import is being applied. Nothing was imported; choose the file again in a few seconds.")
	case errors.Is(err, catalog.ErrIdempotencyConflict):
		ui.importProblem(w, r, http.StatusConflict, "This form was already used for another file or mode. Nothing was imported; choose the file again.")
	case errors.Is(err, catalog.ErrInvalidImportMode), errors.Is(err, catalog.ErrInvalidIdempotencyKey), errors.Is(err, catalog.ErrIdempotencyKeyRequired),
		errors.Is(err, errInvalidAllowDuplicate), errors.Is(err, errImportFieldTooLong):
		ui.importProblem(w, r, http.StatusBadRequest, "The form could not be read. Nothing was imported; choose the file again.")
	default:
		ui.serverError(w, r, err)
	}
}

func (ui *UI) importProblem(w http.ResponseWriter, r *http.Request, status int, message string) {
	ui.page(w, r, status, "admin_import", importPage{Problem: message, Key: uuid.NewString()})
}
