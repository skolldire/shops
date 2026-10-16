package csvimport_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/csvimport"
)

type fakeRows struct {
	calls  int
	file   app.ImportFile
	req    app.ImportRequest
	rows   []app.ImportRow
	report app.ImportReport
	err    error
}

func (f *fakeRows) Import(_ context.Context, req app.ImportRequest) (app.ImportReport, error) {
	f.calls++
	f.file, f.rows, f.req = req.File, req.Rows, req
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
	want := app.ImportReport{Records: 5, Created: 1}
	rows := &fakeRows{report: want}

	report, err := newImporter(t, rows, roomy).Import(context.Background(), strings.NewReader(body), app.ImportOptions{FileName: "products.csv"})

	require.NoError(t, err)
	require.Equal(t, want, report, "the report of the use case is returned as it is")
	require.Equal(t, app.ImportFile{Name: "products.csv", Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, rows.file)
	require.Len(t, rows.rows, 5)
	first := rows.rows[0]
	require.Equal(t, 2, first.Line)
	require.Equal(t, "Shoes", first.Product.Name)
	require.Equal(t, "RS-1", first.Product.SKU)
	require.Equal(t, "Light,\nfast", first.Product.Description)
	require.Equal(t, "Sports", first.Product.Category)
	require.Equal(t, "9.99", first.Product.Price.String())
	require.EqualValues(t, 1, *first.Product.Stock)
	require.Equal(t, "0.3", first.Product.WeightKg.String())
	require.Empty(t, first.Format)
	require.False(t, first.Empty)
	require.Equal(t, []app.ImportRow{
		{Line: 4, Empty: true},
		{Line: 5, Malformed: "must have 7 fields, found 2"},
		{Line: 6, Empty: true},
		{Line: 7, Malformed: "must have 7 fields, found 8"},
	}, rows.rows[1:], "empty rows are flagged as empty and other widths as malformed")
}

func TestImporterReadsNumbersAndReportsTheOnesItCannotRead(t *testing.T) {
	tests := map[string]struct {
		row    string
		format map[string]string
		price  string
		stock  string
		weight string
	}{
		"plain numbers":        {"N,S,,C,9.99,7,0.300", nil, "9.99", "7", "0.3"},
		"spaces are trimmed":   {"N,S,,C, 9.90 , 7 , 0.3 ", nil, "9.9", "7", "0.3"},
		"empty numbers":        {"N,S,,C,,,", nil, "", "", ""},
		"blank numbers":        {"N,S,,C,  , ,\t", nil, "", "", ""},
		"negative values":      {"N,S,,C,-1,-5,-0.1", nil, "-1", "-5", "-0.1"},
		"price with currency":  {"N,S,,C,$9.99,1,1", map[string]string{"price": "invalid_format"}, "", "1", "1"},
		"price as a word":      {"N,S,,C,free,1,1", map[string]string{"price": "invalid_format"}, "", "1", "1"},
		"price with exponent":  {"N,S,,C,1e100000000,1,1", map[string]string{"price": "invalid_format"}, "", "1", "1"},
		"stock with decimals":  {"N,S,,C,1,1.0,1", map[string]string{"stock": "invalid_format"}, "1", "", "1"},
		"stock with plus sign": {"N,S,,C,1,+5,1", map[string]string{"stock": "invalid_format"}, "1", "", "1"},
		"stock with exponent":  {"N,S,,C,1,1e3,1", map[string]string{"stock": "invalid_format"}, "1", "", "1"},
		"stock above int32":    {"N,S,,C,1,2147483648,1", map[string]string{"stock": "out_of_range"}, "1", "", "1"},
		"stock beyond int64":   {"N,S,,C,1,99999999999999999999,1", map[string]string{"stock": "out_of_range"}, "1", "", "1"},
		"weight as a word":     {"N,S,,C,1,1,heavy", map[string]string{"weight_kg": "invalid_format"}, "1", "1", ""},
		"every number invalid": {"N,S,,C,free,ten,heavy", map[string]string{"price": "invalid_format", "stock": "invalid_format", "weight_kg": "invalid_format"}, "", "", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rows := &fakeRows{}

			_, err := newImporter(t, rows, roomy).Import(context.Background(), strings.NewReader(header+tt.row+"\n"), app.ImportOptions{FileName: "products.csv"})

			require.NoError(t, err)
			require.Len(t, rows.rows, 1)
			row := rows.rows[0]
			codes := map[string]string{}
			for _, fe := range row.Format {
				require.NotEmpty(t, fe.Message)
				codes[fe.Field] = fe.Code
			}
			if tt.format == nil {
				require.Empty(t, codes)
			} else {
				require.Equal(t, tt.format, codes)
			}
			text := func(d interface{ String() string }, missing bool) string {
				if missing {
					return ""
				}
				return d.String()
			}
			require.Equal(t, tt.price, text(row.Product.Price, row.Product.Price == nil))
			require.Equal(t, tt.weight, text(row.Product.WeightKg, row.Product.WeightKg == nil))
			if tt.stock == "" {
				require.Nil(t, row.Product.Stock)
			} else {
				require.Equal(t, tt.stock, strconv.FormatInt(*row.Product.Stock, 10))
			}
			require.Equal(t, "N", row.Product.Name, "text fields are handed over as they came")
			require.Equal(t, "S", row.Product.SKU)
		})
	}
}

func TestImporterReturnsTheUseCaseErrorWithItsPartialReport(t *testing.T) {
	partial := app.ImportReport{Records: 1200, Created: 500, Errors: []app.RowError{}}
	rows := &fakeRows{report: partial, err: context.Canceled}

	report, err := newImporter(t, rows, roomy).Import(context.Background(), strings.NewReader(header), app.ImportOptions{FileName: "products.csv"})

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
		"another header":   {"sku,name\n" + row, csvimport.ErrInvalidHeader},
		"empty file":       {"", csvimport.ErrInvalidHeader},
		"broken quotes":    {header + "A,\"A-1,,A,5,1,1\n", csvimport.ErrInvalidCSV},
		"invalid utf-8":    {header + "A\xff,A-1,,A,5,1,1\n", csvimport.ErrInvalidCSV},
		"too many rows":    {header + strings.Repeat(",,,,,,\n", 4), csvimport.ErrTooManyRows},
		"too many bytes":   {header + row + strings.Repeat(" ", 200), csvimport.ErrFileTooLarge},
		"rows before size": {header + strings.Repeat(row, 4), csvimport.ErrTooManyRows},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rows := &fakeRows{}

			report, err := newImporter(t, rows, limits).Import(context.Background(), strings.NewReader(tt.body), app.ImportOptions{FileName: "products.csv"})

			require.ErrorIs(t, err, tt.target)
			require.Equal(t, app.ImportReport{}, report)
			require.Zero(t, rows.calls, "a rejected file never reaches the use case")
		})
	}
}

func TestImporterReportsTheLineOfAMalformedFile(t *testing.T) {
	_, err := newImporter(t, &fakeRows{}, roomy).Import(context.Background(), strings.NewReader(header+"A,A-1,,A,5,1,1\nB,\"B-1,,A,5,1,1\n"), app.ImportOptions{FileName: "products.csv"})

	var invalid *csvimport.FormatError
	require.ErrorAs(t, err, &invalid)
	require.Equal(t, 3, invalid.Line)
	require.NotEmpty(t, invalid.Reason)
}

func TestImporterAcceptsAFileOfExactlyTheLimits(t *testing.T) {
	body := header + "A,A-1,,A,5,1,1\nB,B-1,,A,5,1,1\n"
	rows := &fakeRows{}

	_, err := newImporter(t, rows, csvimport.Limits{MaxBytes: int64(len(body)), MaxRows: 2}).Import(context.Background(), strings.NewReader(body), app.ImportOptions{FileName: "products.csv"})

	require.NoError(t, err)
	require.Len(t, rows.rows, 2)
	require.EqualValues(t, len(body), rows.file.Bytes)
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestImporterReturnsReadFailures(t *testing.T) {
	rows := &fakeRows{}

	_, err := newImporter(t, rows, roomy).Import(context.Background(), brokenReader{}, app.ImportOptions{FileName: "products.csv"})

	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.ErrorContains(t, err, "catalog: read import file")
	require.Zero(t, rows.calls)
}

func TestImporterCleansTheFileName(t *testing.T) {
	rows := &fakeRows{}
	importer := newImporter(t, rows, roomy)

	_, err := importer.Import(context.Background(), strings.NewReader(header), app.ImportOptions{FileName: "pro\tducts\x00.csv"})
	require.NoError(t, err)
	require.Equal(t, "products.csv", rows.file.Name)

	_, err = importer.Import(context.Background(), strings.NewReader(header), app.ImportOptions{FileName: strings.Repeat("ñ", 400) + ".csv"})
	require.NoError(t, err)
	require.Len(t, []rune(rows.file.Name), 255)

	_, err = importer.Import(context.Background(), strings.NewReader(header), app.ImportOptions{FileName: "bad\xff.csv"})
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

func TestIsRejectionTellsFileErrorsFromOtherFailures(t *testing.T) {
	for _, err := range []error{
		csvimport.ErrInvalidHeader, csvimport.ErrInvalidCSV, csvimport.ErrTooManyRows, csvimport.ErrFileTooLarge,
		&csvimport.FormatError{Line: 3, Reason: "bare quote"},
		fmt.Errorf("wrapped: %w", csvimport.ErrTooManyRows),
	} {
		require.True(t, csvimport.IsRejection(err), err.Error())
	}
	for _, err := range []error{nil, context.Canceled, io.ErrUnexpectedEOF, errors.New("boom")} {
		require.False(t, csvimport.IsRejection(err))
	}
}
