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

	"github.com/skolldire/shops/internal/catalog/internal/core"
)

const maxFileNameLength = 255

type RowImporter interface {
	Import(ctx context.Context, file core.ImportFile, rows []core.ImportRow) (core.ImportReport, error)
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

func (i *Importer) Import(ctx context.Context, r io.Reader, fileName string) (core.ImportReport, error) {
	metered := &meteredReader{r: r, hash: sha256.New(), max: i.limits.MaxBytes}
	read, err := Read(metered, i.limits.MaxRows)
	if err != nil {
		return core.ImportReport{}, readError(err)
	}
	rows := make([]core.ImportRow, len(read))
	for index, row := range read {
		rows[index] = importRow(row)
	}
	file := core.ImportFile{
		Name:   cleanFileName(fileName),
		Bytes:  metered.n,
		SHA256: hex.EncodeToString(metered.hash.Sum(nil)),
	}
	return i.rows.Import(ctx, file, rows)
}

func importRow(row Row) core.ImportRow {
	f := row.Fields
	switch {
	case blank(f):
		return core.ImportRow{Line: row.Line}
	case len(f) != len(header):
		return core.ImportRow{Line: row.Line, Malformed: fmt.Sprintf("must have %d fields, found %d", len(header), len(f))}
	}
	return core.ImportRow{Line: row.Line, Fields: core.ImportFields{
		Name: f[0], SKU: f[1], Description: f[2], Category: f[3], Price: f[4], Stock: f[5], WeightKg: f[6],
	}}
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
	case errors.Is(err, core.ErrFileTooLarge):
		return core.ErrFileTooLarge
	case errors.Is(err, ErrInvalidHeader):
		return core.ErrInvalidHeader
	case errors.Is(err, ErrTooManyRows):
		return core.ErrTooManyRows
	case errors.As(err, &format):
		return &core.InvalidCSVError{Line: format.Line, Reason: format.Reason}
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
		return 0, core.ErrFileTooLarge
	}
	_, _ = m.hash.Write(p[:n])
	return n, err
}
