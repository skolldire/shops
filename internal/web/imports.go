package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/skolldire/shops/internal/catalog"
)

const (
	adminImports           = "/admin/imports"
	importField            = "file"
	maxShownImportErrors   = 200
	multipartOverheadBytes = 64 << 10
	megabyte               = 1 << 20
)

type importPage struct {
	Limits  string
	Problem string
	Report  *catalog.ImportReport
	Errors  []catalog.RowError
	More    int
}

func (ui *UI) importPage() importPage {
	return importPage{Limits: fmt.Sprintf("up to %d MB and %d rows", ui.importLimits.MaxBytes/megabyte, ui.importLimits.MaxRows)}
}

func (ui *UI) importForm(w http.ResponseWriter, r *http.Request) {
	ui.page(w, r, http.StatusOK, "admin_import", ui.importPage())
}

func (ui *UI) importFile(w http.ResponseWriter, r *http.Request) {
	parts, err := r.MultipartReader()
	if err != nil {
		ui.importProblem(w, r, http.StatusBadRequest, "Choose a CSV file to import.")
		return
	}
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
		case part.FormName() != importField:
			continue
		}
		report, err := ui.catalog.Import(r.Context(), io.Reader(part), catalog.ImportOptions{FileName: part.FileName()})
		if err != nil {
			ui.importFailed(w, r, err)
			return
		}
		data := ui.importPage()
		data.Report = &report
		data.Errors = report.Errors
		if len(data.Errors) > maxShownImportErrors {
			data.Errors = data.Errors[:maxShownImportErrors]
			data.More = len(report.Errors) - maxShownImportErrors
		}
		ui.page(w, r, http.StatusOK, "admin_import", data)
		return
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
	default:
		ui.serverError(w, r, err)
	}
}

func (ui *UI) importProblem(w http.ResponseWriter, r *http.Request, status int, message string) {
	data := ui.importPage()
	data.Problem = message
	ui.page(w, r, status, "admin_import", data)
}
