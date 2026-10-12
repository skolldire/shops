package csvimport_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/catalog/internal/csvimport"
)

type fakeRows struct {
	calls  int
	file   core.ImportFile
	rows   []core.ImportRow
	report core.ImportReport
	err    error
}

func (f *fakeRows) Import(_ context.Context, file core.ImportFile, rows []core.ImportRow) (core.ImportReport, error) {
	f.calls++
	f.file, f.rows = file, rows
	return f.report, f.err
}

func newImporter(t *testing.T, rows *fakeRows, limits csvimport.Limits) *csvimport.Importer {
	t.Helper()
	importer, err := csvimport.NewImporter(rows, limits)
	require.NoError(t, err)
	return importer
}

var roomy = csvimport.Limits{MaxBytes: 1 << 20, MaxRows: 1000}

func TestImporterHandsTheRowsAndTheFileToTheUseCase(t *testing.T) {
	body := header + "Shoes,RS-1,\"Light,\nfast\",Sports,9.99,1,0.3\n,,,,,,\nMug,MG-1\n , \nA,B,C,D,E,F,G,H\n"
	sum := sha256.Sum256([]byte(body))
	want := core.ImportReport{Records: 5, Created: 1}
	rows := &fakeRows{report: want}

	report, err := newImporter(t, rows, roomy).Import(context.Background(), strings.NewReader(body), "products.csv")

	require.NoError(t, err)
	require.Equal(t, want, report, "the report of the use case is returned as it is")
	require.Equal(t, core.ImportFile{Name: "products.csv", Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, rows.file)
	require.Equal(t, []core.ImportRow{
		{Line: 2, Fields: core.ImportFields{
			Name: "Shoes", SKU: "RS-1", Description: "Light,\nfast", Category: "Sports", Price: "9.99", Stock: "1", WeightKg: "0.3",
		}},
		{Line: 4},
		{Line: 5, Malformed: "must have 7 fields, found 2"},
		{Line: 6},
		{Line: 7, Malformed: "must have 7 fields, found 8"},
	}, rows.rows, "columns are mapped by name, empty rows stay empty and other widths are flagged")
}

func TestImporterReturnsTheUseCaseErrorWithItsPartialReport(t *testing.T) {
	partial := core.ImportReport{Records: 1200, Created: 500, Errors: []core.RowError{}}
	rows := &fakeRows{report: partial, err: context.Canceled}

	report, err := newImporter(t, rows, roomy).Import(context.Background(), strings.NewReader(header), "products.csv")

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, partial, report)
}

func TestImporterRejectsTheWholeFile(t *testing.T) {
	limits := csvimport.Limits{MaxBytes: 200, MaxRows: 3}
	row := "A,A-1,,A,5,1,1\n"
	tests := map[string]struct {
		body   string
		target error
	}{
		"another header":   {"sku,name\n" + row, core.ErrInvalidHeader},
		"empty file":       {"", core.ErrInvalidHeader},
		"broken quotes":    {header + "A,\"A-1,,A,5,1,1\n", core.ErrInvalidCSV},
		"invalid utf-8":    {header + "A\xff,A-1,,A,5,1,1\n", core.ErrInvalidCSV},
		"too many rows":    {header + strings.Repeat(",,,,,,\n", 4), core.ErrTooManyRows},
		"too many bytes":   {header + row + strings.Repeat(" ", 200), core.ErrFileTooLarge},
		"rows before size": {header + strings.Repeat(row, 4), core.ErrTooManyRows},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rows := &fakeRows{}

			report, err := newImporter(t, rows, limits).Import(context.Background(), strings.NewReader(tt.body), "products.csv")

			require.ErrorIs(t, err, tt.target)
			require.Equal(t, core.ImportReport{}, report)
			require.Zero(t, rows.calls, "a rejected file never reaches the use case")
		})
	}
}

func TestImporterReportsTheLineOfAMalformedFile(t *testing.T) {
	_, err := newImporter(t, &fakeRows{}, roomy).Import(context.Background(), strings.NewReader(header+"A,A-1,,A,5,1,1\nB,\"B-1,,A,5,1,1\n"), "products.csv")

	var invalid *core.InvalidCSVError
	require.ErrorAs(t, err, &invalid)
	require.Equal(t, 3, invalid.Line)
	require.NotEmpty(t, invalid.Reason)
}

func TestImporterAcceptsAFileOfExactlyTheLimits(t *testing.T) {
	body := header + "A,A-1,,A,5,1,1\nB,B-1,,A,5,1,1\n"
	rows := &fakeRows{}

	_, err := newImporter(t, rows, csvimport.Limits{MaxBytes: int64(len(body)), MaxRows: 2}).Import(context.Background(), strings.NewReader(body), "products.csv")

	require.NoError(t, err)
	require.Len(t, rows.rows, 2)
	require.EqualValues(t, len(body), rows.file.Bytes)
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestImporterReturnsReadFailures(t *testing.T) {
	rows := &fakeRows{}

	_, err := newImporter(t, rows, roomy).Import(context.Background(), brokenReader{}, "products.csv")

	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.ErrorContains(t, err, "catalog: read import file")
	require.Zero(t, rows.calls)
}

func TestImporterCleansTheFileName(t *testing.T) {
	rows := &fakeRows{}
	importer := newImporter(t, rows, roomy)

	_, err := importer.Import(context.Background(), strings.NewReader(header), "pro\tducts\x00.csv")
	require.NoError(t, err)
	require.Equal(t, "products.csv", rows.file.Name)

	_, err = importer.Import(context.Background(), strings.NewReader(header), strings.Repeat("ñ", 400)+".csv")
	require.NoError(t, err)
	require.Len(t, []rune(rows.file.Name), 255)

	_, err = importer.Import(context.Background(), strings.NewReader(header), "bad\xff.csv")
	require.NoError(t, err)
	require.Equal(t, "bad.csv", rows.file.Name)
}

func TestNewImporterRequiresItsDependencies(t *testing.T) {
	_, err := csvimport.NewImporter(nil, roomy)
	require.EqualError(t, err, "catalog: csv importer requires a row importer")
	_, err = csvimport.NewImporter(&fakeRows{}, csvimport.Limits{MaxBytes: 1})
	require.EqualError(t, err, "catalog: csv importer requires positive limits")
	_, err = csvimport.NewImporter(&fakeRows{}, csvimport.Limits{MaxRows: 1})
	require.EqualError(t, err, "catalog: csv importer requires positive limits")
}

func TestImporterErrorsAreTheOnesTheUseCaseDeclares(t *testing.T) {
	_, err := newImporter(t, &fakeRows{}, roomy).Import(context.Background(), strings.NewReader("sku,name\n"), "products.csv")

	require.True(t, errors.Is(err, core.ErrInvalidHeader))
	require.False(t, errors.Is(err, csvimport.ErrInvalidHeader), "the adapter's own error does not leak")
}
