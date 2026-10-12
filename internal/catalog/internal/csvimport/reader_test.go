package csvimport_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/csvimport"
)

const header = "name,sku,description,category,price,stock,weight_kg\n"

func TestReadRows(t *testing.T) {
	tests := map[string]struct {
		input string
		want  []csvimport.Row
	}{
		"header only": {header, nil},
		"plain rows": {
			header + "Shoes,RS-1,Light,Sports,9.99,1,0.3\nMug,MG-1,,Home,2,0,0\n",
			[]csvimport.Row{
				{Line: 2, Fields: []string{"Shoes", "RS-1", "Light", "Sports", "9.99", "1", "0.3"}},
				{Line: 3, Fields: []string{"Mug", "MG-1", "", "Home", "2", "0", "0"}},
			},
		},
		"utf-8 bom is tolerated": {
			string([]byte{0xEF, 0xBB, 0xBF}) + header + "Shoes,RS-1,Light,Sports,9.99,1,0.3\n",
			[]csvimport.Row{{Line: 2, Fields: []string{"Shoes", "RS-1", "Light", "Sports", "9.99", "1", "0.3"}}},
		},
		"crlf and no trailing newline": {
			"name,sku,description,category,price,stock,weight_kg\r\nShoes,RS-1,Light,Sports,9.99,1,0.3",
			[]csvimport.Row{{Line: 2, Fields: []string{"Shoes", "RS-1", "Light", "Sports", "9.99", "1", "0.3"}}},
		},
		"quotes, commas and escaped quotes": {
			header + `"Mug, large",MG-1,"He said ""hi""",Home,2,0,0` + "\n",
			[]csvimport.Row{{Line: 2, Fields: []string{"Mug, large", "MG-1", `He said "hi"`, "Home", "2", "0", "0"}}},
		},
		"multiline field keeps the line where each record starts": {
			header + "Tent,CT-1,\"line one\nline two\nline three\",Outdoors,5,1,1\nMug,MG-1,,Home,2,0,0\n",
			[]csvimport.Row{
				{Line: 2, Fields: []string{"Tent", "CT-1", "line one\nline two\nline three", "Outdoors", "5", "1", "1"}},
				{Line: 5, Fields: []string{"Mug", "MG-1", "", "Home", "2", "0", "0"}},
			},
		},
		"rows with another number of fields are returned as they are": {
			header + "Shoes,RS-1\nMug,MG-1,,Home,2,0,0,extra\n,,,,,,\n",
			[]csvimport.Row{
				{Line: 2, Fields: []string{"Shoes", "RS-1"}},
				{Line: 3, Fields: []string{"Mug", "MG-1", "", "Home", "2", "0", "0", "extra"}},
				{Line: 4, Fields: []string{"", "", "", "", "", "", ""}},
			},
		},
		"spaces are not trimmed": {
			header + " Shoes ,rs-1 ,,Sports, 9.99,1,0.3\n",
			[]csvimport.Row{{Line: 2, Fields: []string{" Shoes ", "rs-1 ", "", "Sports", " 9.99", "1", "0.3"}}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rows, err := csvimport.Read(strings.NewReader(tt.input), 100)

			require.NoError(t, err)
			require.Equal(t, tt.want, rows)
		})
	}
}

func TestReadRejectsTheWholeFile(t *testing.T) {
	tests := map[string]struct {
		input  string
		target error
		line   int
	}{
		"empty file":              {"", csvimport.ErrInvalidHeader, 0},
		"header in another order": {"sku,name,description,category,price,stock,weight_kg\n", csvimport.ErrInvalidHeader, 0},
		"header with other case":  {"Name,SKU,description,category,price,stock,weight_kg\n", csvimport.ErrInvalidHeader, 0},
		"header with spaces":      {"name, sku,description,category,price,stock,weight_kg\n", csvimport.ErrInvalidHeader, 0},
		"header missing a column": {"name,sku,description,category,price,stock\n", csvimport.ErrInvalidHeader, 0},
		"header with extra":       {"name,sku,description,category,price,stock,weight_kg,color\n", csvimport.ErrInvalidHeader, 0},
		"data without a header":   {"Shoes,RS-1,Light,Sports,9.99,1,0.3\n", csvimport.ErrInvalidHeader, 0},
		"quote never closed":      {header + "Shoes,RS-1,\"Light,Sports,9.99,1,0.3\n", csvimport.ErrInvalidCSV, 2},
		"bare quote":              {header + "Mug,MG-1,,Home,2,0,0\nSho\"es,RS-1,Light,Sports,9.99,1,0.3\n", csvimport.ErrInvalidCSV, 3},
		"invalid utf-8 in a row":  {header + "Mug,MG-1,,Home,2,0,0\nSh\xffoes,RS-1,Light,Sports,9.99,1,0.3\n", csvimport.ErrInvalidCSV, 3},
		"invalid utf-8 in header": {"na\xffme,sku,description,category,price,stock,weight_kg\n", csvimport.ErrInvalidCSV, 1},
		"more rows than allowed":  {header + strings.Repeat("Mug,MG-1,,Home,2,0,0\n", 101), csvimport.ErrTooManyRows, 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rows, err := csvimport.Read(strings.NewReader(tt.input), 100)

			require.ErrorIs(t, err, tt.target)
			require.Nil(t, rows)
			var format *csvimport.FormatError
			if tt.line > 0 {
				require.ErrorAs(t, err, &format)
				require.Equal(t, tt.line, format.Line)
				require.NotEmpty(t, format.Reason)
			}
		})
	}
}

func TestReadAllowsExactlyTheRowLimit(t *testing.T) {
	rows, err := csvimport.Read(strings.NewReader(header+strings.Repeat(",,,,,,\n", 100)), 100)

	require.NoError(t, err)
	require.Len(t, rows, 100)
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestReadReturnsReaderErrorsUntouched(t *testing.T) {
	boom := errors.New("boom")

	_, err := csvimport.Read(failingReader{err: boom}, 100)

	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, csvimport.ErrInvalidCSV)
}
