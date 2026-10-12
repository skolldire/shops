package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/platform/validation"
)

const (
	CodeWrongFieldCount    = "wrong_field_count"
	CodeDuplicateInFile    = "duplicate_in_file"
	CodeSKUDeleted         = "sku_deleted"
	CodeRejectedByDatabase = "rejected_by_database"

	OutcomeCreated   UpsertOutcome = "created"
	OutcomeUpdated   UpsertOutcome = "updated"
	OutcomeUnchanged UpsertOutcome = "unchanged"
	OutcomeDeleted   UpsertOutcome = "deleted"

	outcomeSkipped  = "skipped"
	outcomeRejected = "rejected"

	rowField        = "row"
	importBatchSize = 500
	importColumns   = 7
)

var (
	ErrInvalidHeader = errors.New("catalog: the csv header must be exactly name,sku,description,category,price,stock,weight_kg")
	ErrInvalidCSV    = errors.New("catalog: invalid csv")
	ErrTooManyRows   = errors.New("catalog: the csv has too many rows")
	ErrFileTooLarge  = errors.New("catalog: the csv file is too large")
)

type UpsertOutcome string

type InvalidCSVError struct {
	Line   int
	Reason string
}

func (e *InvalidCSVError) Error() string {
	return fmt.Sprintf("catalog: invalid csv on line %d: %s", e.Line, e.Reason)
}

func (e *InvalidCSVError) Is(target error) bool {
	return target == ErrInvalidCSV
}

type ImportRow struct {
	Line   int
	Fields []string
}

type ImportFile struct {
	Name   string
	Bytes  int64
	SHA256 string
}

type RowError struct {
	Line    int
	SKU     string
	Field   string
	Code    string
	Message string
}

type ImportReport struct {
	File      ImportFile
	Records   int
	Skipped   int
	Created   int
	Updated   int
	Unchanged int
	Rejected  int
	Errors    []RowError
}

type Importer struct {
	store    ProductUpserter
	rec      ImportRecorder
	failures ImportFailureReporter
}

func NewImporter(store ProductUpserter, rec ImportRecorder, failures ImportFailureReporter) (*Importer, error) {
	switch {
	case store == nil:
		return nil, errors.New("catalog: importer requires a product store")
	case rec == nil:
		return nil, errors.New("catalog: importer requires a recorder")
	case failures == nil:
		return nil, errors.New("catalog: importer requires a failure reporter")
	}
	return &Importer{store: store, rec: rec, failures: failures}, nil
}

type importRow struct {
	line    int
	sku     string
	product Product
	errs    validation.Errors
}

func (i *Importer) Import(ctx context.Context, file ImportFile, rows []ImportRow) (ImportReport, error) {
	start := time.Now()
	report := ImportReport{File: file, Records: len(rows), Errors: []RowError{}}
	valid := i.validate(rows, &report)
	slices.SortFunc(valid, func(a, b importRow) int { return cmp.Compare(a.sku, b.sku) })
	var interrupted error
	for batch := range slices.Chunk(valid, importBatchSize) {
		if interrupted = i.apply(ctx, batch, &report); interrupted != nil {
			break
		}
	}
	slices.SortFunc(report.Errors, func(a, b RowError) int {
		return cmp.Or(cmp.Compare(a.Line, b.Line), cmp.Compare(a.Field, b.Field), cmp.Compare(a.Code, b.Code))
	})

	for outcome, n := range map[string]int{
		string(OutcomeCreated):   report.Created,
		string(OutcomeUpdated):   report.Updated,
		string(OutcomeUnchanged): report.Unchanged,
		outcomeSkipped:           report.Skipped,
		outcomeRejected:          report.Rejected,
	} {
		i.rec.RecordRows(ctx, outcome, n)
	}
	i.rec.RecordDuration(ctx, time.Since(start))
	return report, interrupted
}

func (i *Importer) validate(rows []ImportRow, report *ImportReport) []importRow {
	candidates := make([]importRow, 0, len(rows))
	bySKU := map[string][]int{}
	for _, row := range rows {
		if isEmpty(row.Fields) {
			report.Skipped++
			continue
		}
		if len(row.Fields) != importColumns {
			report.Rejected++
			report.Errors = append(report.Errors, RowError{
				Line: row.Line, Field: rowField, Code: CodeWrongFieldCount,
				Message: fmt.Sprintf("must have %d fields, found %d", importColumns, len(row.Fields)),
			})
			continue
		}
		candidate := newImportRow(row)
		if candidate.sku != "" {
			bySKU[candidate.sku] = append(bySKU[candidate.sku], len(candidates))
		}
		candidates = append(candidates, candidate)
	}

	for _, group := range bySKU {
		if len(group) < 2 {
			continue
		}
		for _, index := range group {
			candidates[index].errs.Add("sku", CodeDuplicateInFile, "also used on "+otherLines(candidates, group, index))
		}
	}

	valid := make([]importRow, 0, len(candidates))
	for _, candidate := range candidates {
		if len(candidate.errs) == 0 {
			valid = append(valid, candidate)
			continue
		}
		report.Rejected++
		for _, fe := range candidate.errs {
			report.Errors = append(report.Errors, RowError{
				Line: candidate.line, SKU: candidate.sku, Field: fe.Field, Code: fe.Code, Message: fe.Message,
			})
		}
	}
	return valid
}

func (i *Importer) apply(ctx context.Context, batch []importRow, report *ImportReport) error {
	products := make([]Product, len(batch))
	for index, row := range batch {
		products[index] = row.product
	}
	outcomes, err := i.store.Upsert(ctx, products)
	if err == nil {
		for _, row := range batch {
			count(report, row, outcomes[row.sku])
		}
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("catalog: import interrupted: %w", ctx.Err())
	}
	i.failures.BatchRetried(ctx, len(batch), err)
	for _, row := range batch {
		outcomes, err := i.store.Upsert(ctx, []Product{row.product})
		if err == nil {
			count(report, row, outcomes[row.sku])
			continue
		}
		if ctx.Err() != nil {
			return fmt.Errorf("catalog: import interrupted: %w", ctx.Err())
		}
		i.failures.RowRejected(ctx, row.line, row.sku, err)
		report.Rejected++
		report.Errors = append(report.Errors, RowError{
			Line: row.line, SKU: row.sku, Field: rowField, Code: CodeRejectedByDatabase, Message: "could not be saved",
		})
	}
	return nil
}

func count(report *ImportReport, row importRow, outcome UpsertOutcome) {
	switch outcome {
	case OutcomeCreated:
		report.Created++
	case OutcomeUpdated:
		report.Updated++
	case OutcomeDeleted:
		report.Rejected++
		report.Errors = append(report.Errors, RowError{
			Line: row.line, SKU: row.sku, Field: "sku", Code: CodeSKUDeleted,
			Message: "belongs to a deleted product and cannot be reused",
		})
	default:
		report.Unchanged++
	}
}

func newImportRow(row ImportRow) importRow {
	var format validation.Errors
	f := row.Fields
	product, err := NewProduct(NewProductInput{
		Name: f[0], SKU: f[1], Description: f[2], Category: f[3],
		Price:    decimalCell(&format, "price", f[4]),
		Stock:    wholeCell(&format, "stock", f[5]),
		WeightKg: decimalCell(&format, "weight_kg", f[6]),
	})
	return importRow{line: row.Line, sku: NormalizeSKU(f[1]), product: product, errs: rowErrors(format, err)}
}

func NormalizeSKU(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}

func decimalCell(errs *validation.Errors, field, raw string) *decimal.Decimal {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	d, err := ParseDecimal(raw)
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
	n, err := ParseWholeNumber(raw)
	if err != nil {
		errs.Add(field, WholeNumberErrorCode(err), err.Error())
		return nil
	}
	return &n
}

func rowErrors(format validation.Errors, domain error) validation.Errors {
	var errs validation.Errors
	errors.As(validation.Merge(format, domain), &errs)
	return errs
}

func isEmpty(fields []string) bool {
	for _, field := range fields {
		if strings.TrimSpace(field) != "" {
			return false
		}
	}
	return true
}

func otherLines(candidates []importRow, group []int, self int) string {
	lines := make([]string, 0, len(group)-1)
	for _, index := range group {
		if index != self {
			lines = append(lines, strconv.Itoa(candidates[index].line))
		}
	}
	if len(lines) == 1 {
		return "line " + lines[0]
	}
	return "lines " + strings.Join(lines[:len(lines)-1], ", ") + " and " + lines[len(lines)-1]
}
