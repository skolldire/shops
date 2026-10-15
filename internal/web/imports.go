package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/skolldire/shops/internal/catalog"
)

const (
	adminImports         = "/admin/imports"
	importField          = "file"
	maxShownImportErrors = 200
	maxShownPreviewRows  = 200
	maxImportFieldBytes  = 64
)

type importForm struct {
	Problem string
	Key     string
}

type importPage struct {
	Import  catalog.ImportPreview
	Errors  []catalog.RowError
	More    int
	Rows    []catalog.PreviewRow
	MoreRow int
	Problem string
}

func (ui *UI) importForm(w http.ResponseWriter, r *http.Request) {
	ui.page(w, r, http.StatusOK, "admin_import", importForm{Key: uuid.NewString()})
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
			ui.importFailed(w, r, err)
			return
		case err != nil:
			ui.importProblem(w, r, http.StatusBadRequest, "Choose a CSV file to import.")
			return
		}
		switch part.FormName() {
		case "mode", "idempotency_key":
			value, err := io.ReadAll(io.LimitReader(part, maxImportFieldBytes))
			if err != nil {
				ui.importFailed(w, r, err)
				return
			}
			if part.FormName() == "mode" {
				opts.Mode = catalog.ImportMode(strings.TrimSpace(string(value)))
			} else {
				opts.IdempotencyKey = strings.TrimSpace(string(value))
			}
		case importField:
			if opts.Mode != catalog.ImportAdd && opts.Mode != catalog.ImportReplace {
				ui.importProblem(w, r, http.StatusBadRequest, "Choose whether to add to or replace the inventory before the file.")
				return
			}
			opts.FileName = part.FileName()
			id, err := ui.catalog.PreviewImport(r.Context(), part, opts)
			if err != nil {
				ui.importFailed(w, r, err)
				return
			}
			ui.redirectToImport(w, r, id)
			return
		}
	}
}

func (ui *UI) redirectToImport(w http.ResponseWriter, r *http.Request, id string) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		ui.notFound(w, r)
		return
	}
	http.Redirect(w, r, adminImports+"/"+parsed.String(), http.StatusSeeOther)
}

func (ui *UI) importStatus(w http.ResponseWriter, r *http.Request) {
	ui.showImport(w, r, http.StatusOK, chi.URLParam(r, "id"), "")
}

func (ui *UI) showImport(w http.ResponseWriter, r *http.Request, status int, id, problem string) {
	preview, err := ui.catalog.ImportPreview(r.Context(), id)
	switch {
	case errors.Is(err, catalog.ErrImportNotFound):
		ui.notFound(w, r)
		return
	case err != nil:
		ui.serverError(w, r, err)
		return
	}
	data := importPage{Import: preview, Errors: preview.Report.Errors, Rows: preview.Rows, Problem: problem}
	if len(data.Errors) > maxShownImportErrors {
		data.Errors, data.More = data.Errors[:maxShownImportErrors], len(preview.Report.Errors)-maxShownImportErrors
	}
	if len(data.Rows) > maxShownPreviewRows {
		data.Rows, data.MoreRow = data.Rows[:maxShownPreviewRows], len(preview.Rows)-maxShownPreviewRows
	}
	ui.page(w, r, status, "admin_import_status", data)
}

func (ui *UI) confirmImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		ui.badForm(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	_, err := ui.catalog.ConfirmImport(r.Context(), id, r.PostForm.Get("allow_duplicate") == "true")
	switch {
	case err == nil:
		ui.redirectToImport(w, r, id)
	case errors.Is(err, catalog.ErrImportNotFound):
		ui.notFound(w, r)
	case errors.Is(err, catalog.ErrPreviewExpired):
		ui.showImport(w, r, http.StatusGone, id, "This preview expired. Upload the file again to import it.")
	case errors.Is(err, catalog.ErrDuplicateFile):
		ui.showImport(w, r, http.StatusConflict, id, "Tick the confirmation to add the same file again.")
	case errors.Is(err, catalog.ErrImportCancelled):
		ui.showImport(w, r, http.StatusConflict, id, "This import was cancelled. Upload the file again to import it.")
	default:
		ui.serverError(w, r, err)
	}
}

func (ui *UI) cancelImport(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	err := ui.catalog.CancelImport(r.Context(), id)
	switch {
	case err == nil:
		ui.redirectToImport(w, r, id)
	case errors.Is(err, catalog.ErrImportNotFound):
		ui.notFound(w, r)
	case errors.Is(err, catalog.ErrImportNotCancellable):
		ui.showImport(w, r, http.StatusConflict, id, "This import is already being applied or was applied, so it cannot be cancelled.")
	default:
		ui.serverError(w, r, err)
	}
}

func (ui *UI) importFailed(w http.ResponseWriter, r *http.Request, err error) {
	var (
		tooLarge *http.MaxBytesError
		invalid  *catalog.InvalidCSVError
	)
	switch {
	case errors.Is(err, catalog.ErrFileTooLarge), errors.As(err, &tooLarge):
		ui.importProblem(w, r, http.StatusRequestEntityTooLarge, "The file is larger than the import limit. Nothing was imported.")
	case errors.Is(err, catalog.ErrInvalidHeader):
		ui.importProblem(w, r, http.StatusBadRequest, "The first line must be exactly name,sku,description,category,price,stock,weight_kg. Nothing was imported.")
	case errors.As(err, &invalid):
		ui.importProblem(w, r, http.StatusBadRequest, fmt.Sprintf("The file is not a valid UTF-8 CSV. Line %d: %s. Nothing was imported.", invalid.Line, invalid.Reason))
	case errors.Is(err, catalog.ErrTooManyRows):
		ui.importProblem(w, r, http.StatusBadRequest, "The file has more rows than the import limit. Nothing was imported.")
	case errors.Is(err, catalog.ErrTooManyPreviews):
		ui.importProblem(w, r, http.StatusTooManyRequests, "Too many imports are waiting for a confirmation. Confirm or cancel one, or try again later.")
	case errors.Is(err, catalog.ErrIdempotencyConflict):
		ui.importProblem(w, r, http.StatusConflict, "This form was already used for another file. Choose the file again.")
	case errors.Is(err, catalog.ErrInvalidImportMode), errors.Is(err, catalog.ErrInvalidIdempotencyKey):
		ui.importProblem(w, r, http.StatusBadRequest, "The form could not be read. Choose the file again.")
	default:
		ui.serverError(w, r, err)
	}
}

func (ui *UI) importProblem(w http.ResponseWriter, r *http.Request, status int, message string) {
	ui.page(w, r, status, "admin_import", importForm{Problem: message, Key: uuid.NewString()})
}
