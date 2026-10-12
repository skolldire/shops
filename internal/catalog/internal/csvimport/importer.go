package csvimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"unicode"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/validation"
)

const maxFileNameLength = 255

type RowImporter interface {
	Import(ctx context.Context, file app.ImportFile, rows []app.ImportRow) (app.ImportReport, error)
}

type Limits struct {
	MaxBytes int64
	MaxRows  int
}

type Importer struct {
	rows   RowImporter
	limits Limits
}

func NewImporter(rows RowImporter, limits Limits) (*Importer, error) {
	switch {
	case rows == nil:
		return nil, errors.New("catalog: csv importer requires a row importer")
	case limits.MaxBytes <= 0 || limits.MaxRows <= 0:
		return nil, errors.New("catalog: csv importer requires positive limits")
	}
	return &Importer{rows: rows, limits: limits}, nil
}

func (i *Importer) Import(ctx context.Context, r io.Reader, fileName string) (app.ImportReport, error) {
	metered := &meteredReader{r: r, hash: sha256.New(), max: i.limits.MaxBytes}
	read, err := Read(metered, i.limits.MaxRows)
	if err != nil {
		return app.ImportReport{}, readError(err)
	}
	rows := make([]app.ImportRow, len(read))
	for index, row := range read {
		rows[index] = importRow(row)
	}
	file := app.ImportFile{
		Name:   cleanFileName(fileName),
		Bytes:  metered.n,
		SHA256: hex.EncodeToString(metered.hash.Sum(nil)),
	}
	return i.rows.Import(ctx, file, rows)
}

func importRow(row Row) app.ImportRow {
	f := row.Fields
	switch {
	case blank(f):
		return app.ImportRow{Line: row.Line, Empty: true}
	case len(f) != len(header):
		return app.ImportRow{Line: row.Line, Malformed: fmt.Sprintf("must have %d fields, found %d", len(header), len(f))}
	}
	var format validation.Errors
	product := domain.NewProductInput{
		Name: f[0], SKU: f[1], Description: f[2], Category: f[3],
		Price:    decimalCell(&format, "price", f[4]),
		Stock:    wholeCell(&format, "stock", f[5]),
		WeightKg: decimalCell(&format, "weight_kg", f[6]),
	}
	return app.ImportRow{Line: row.Line, Product: product, Format: format}
}

func decimalCell(errs *validation.Errors, field, raw string) *decimal.Decimal {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	d, err := domain.ParseDecimal(raw)
	if err != nil {
		errs.Add(field, validation.CodeInvalidFormat, err.Error())
		return nil
	}
	return &d
}

func wholeCell(errs *validation.Errors, field, raw string) *int64 {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	n, err := domain.ParseWholeNumber(raw)
	if err != nil {
		errs.Add(field, domain.WholeNumberErrorCode(err), err.Error())
		return nil
	}
	return &n
}

func blank(fields []string) bool {
	for _, field := range fields {
		if strings.TrimSpace(field) != "" {
			return false
		}
	}
	return true
}

func cleanFileName(raw string) string {
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(raw, ""))
	if runes := []rune(name); len(runes) > maxFileNameLength {
		name = string(runes[:maxFileNameLength])
	}
	return name
}

func readError(err error) error {
	var format *FormatError
	switch {
	case errors.Is(err, app.ErrFileTooLarge):
		return app.ErrFileTooLarge
	case errors.Is(err, ErrInvalidHeader):
		return app.ErrInvalidHeader
	case errors.Is(err, ErrTooManyRows):
		return app.ErrTooManyRows
	case errors.As(err, &format):
		return &app.InvalidCSVError{Line: format.Line, Reason: format.Reason}
	}
	return fmt.Errorf("catalog: read import file: %w", err)
}

type meteredReader struct {
	r    io.Reader
	hash hash.Hash
	n    int64
	max  int64
}

func (m *meteredReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	m.n += int64(n)
	if m.n > m.max {
		return 0, app.ErrFileTooLarge
	}
	_, _ = m.hash.Write(p[:n])
	return n, err
}
