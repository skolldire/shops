package csvimport

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"
)

var (
	ErrInvalidHeader = errors.New("csvimport: the header must be exactly name,sku,description,category,price,stock,weight_kg")
	ErrInvalidCSV    = errors.New("csvimport: invalid csv")
	ErrTooManyRows   = errors.New("csvimport: too many rows")

	header = []string{"name", "sku", "description", "category", "price", "stock", "weight_kg"}
	bom    = []byte{0xEF, 0xBB, 0xBF}
)

type Row struct {
	Line   int
	Fields []string
}

type FormatError struct {
	Line   int
	Reason string
}

func (e *FormatError) Error() string {
	return fmt.Sprintf("csvimport: line %d: %s", e.Line, e.Reason)
}

func (e *FormatError) Is(target error) bool {
	return target == ErrInvalidCSV
}

func Columns() int {
	return len(header)
}

func Read(r io.Reader, maxRows int) ([]Row, error) {
	buffered := bufio.NewReader(r)
	if start, err := buffered.Peek(len(bom)); err == nil && bytes.Equal(start, bom) {
		_, _ = buffered.Discard(len(bom))
	}
	reader := csv.NewReader(buffered)
	reader.FieldsPerRecord = -1

	first, _, err := next(reader)
	switch {
	case errors.Is(err, io.EOF):
		return nil, ErrInvalidHeader
	case err != nil:
		return nil, err
	case !slices.Equal(first, header):
		return nil, ErrInvalidHeader
	}

	var rows []Row
	for {
		fields, line, err := next(reader)
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		if len(rows) == maxRows {
			return nil, ErrTooManyRows
		}
		rows = append(rows, Row{Line: line, Fields: fields})
	}
}

func next(reader *csv.Reader) ([]string, int, error) {
	fields, err := reader.Read()
	var parseErr *csv.ParseError
	switch {
	case errors.As(err, &parseErr):
		return nil, 0, &FormatError{Line: parseErr.StartLine, Reason: parseErr.Err.Error()}
	case err != nil:
		return nil, 0, err
	}
	line, _ := reader.FieldPos(0)
	for _, field := range fields {
		if !utf8.ValidString(field) {
			return nil, 0, &FormatError{Line: line, Reason: "the file is not valid UTF-8"}
		}
	}
	return fields, line, nil
}
