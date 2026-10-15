package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
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

	OutcomeBelowReserved UpsertOutcome = CodeBelowReserved

	outcomeSkipped  = "skipped"
	outcomeRejected = "rejected"

	rowField        = "row"
	importBatchSize = 500
)

type UpsertOutcome string

type ImportRow struct {
	Line      int
	Empty     bool
	Malformed string
	Product   domain.NewProductInput
	Format    validation.Errors
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
	ID             string
	Mode           domain.ImportMode
	PreviewChanged bool
	File           ImportFile
	Records        int
	Skipped        int
	Created        int
	Updated        int
	Unchanged      int
	Rejected       int
	Errors         []RowError
}

type ImportOptions struct {
	FileName string
	Mode     domain.ImportMode
	Key      string
}

type ImportRequest struct {
	File    ImportFile
	Content []byte
	Rows    []ImportRow
	Mode    domain.ImportMode
	Key     string
}

type Importer struct {
	store    ProductUpserter
	log      ImportLog
	tx       Tx
	rec      ImportRecorder
	failures ImportFailureReporter
}

func NewImporter(store ProductUpserter, log ImportLog, tx Tx, rec ImportRecorder, failures ImportFailureReporter) (*Importer, error) {
	switch {
	case store == nil:
		return nil, errors.New("catalog: importer requires a product store")
	case log == nil:
		return nil, errors.New("catalog: importer requires an import log")
	case tx == nil:
		return nil, errors.New("catalog: importer requires a transaction manager")
	case rec == nil:
		return nil, errors.New("catalog: importer requires a recorder")
	case failures == nil:
		return nil, errors.New("catalog: importer requires a failure reporter")
	}
	return &Importer{store: store, log: log, tx: tx, rec: rec, failures: failures}, nil
}

type importRow struct {
	line    int
	sku     string
	product domain.Product
	errs    validation.Errors
}

func (i *Importer) Import(ctx context.Context, req ImportRequest) (ImportReport, error) {
	if req.Mode == domain.ImportAdd {
		return i.importAdd(ctx, req)
	}
	return i.importReplace(ctx, req)
}

func (i *Importer) importReplace(ctx context.Context, req ImportRequest) (ImportReport, error) {
	report, valid := i.prepare(req)
	rec, _, err := i.log.Create(ctx, ImportRecord{Mode: req.Mode, Status: domain.ImportApplying, File: req.File, Content: req.Content})
	if err != nil {
		i.close(ctx, &report)
		return report, err
	}
	report.ID = rec.ID
	var interrupted error
	for batch := range slices.Chunk(valid, importBatchSize) {
		if interrupted = i.apply(ctx, rec.ID, batch, &report); interrupted != nil {
			break
		}
	}
	i.close(ctx, &report)
	if interrupted != nil {
		return report, interrupted
	}
	if _, err := i.log.Finish(ctx, rec.ID, domain.ImportApplying, report); err != nil {
		return report, err
	}
	return report, nil
}

func (i *Importer) importAdd(ctx context.Context, req ImportRequest) (ImportReport, error) {
	var report ImportReport
	err := i.tx.WithinTx(ctx, func(ctx context.Context) error {
		rec, _, err := i.log.Create(ctx, ImportRecord{Mode: req.Mode, Status: domain.ImportPreview, File: req.File})
		if err != nil {
			return err
		}
		var valid []importRow
		report, valid = i.prepare(req)
		report.ID = rec.ID
		for batch := range slices.Chunk(valid, importBatchSize) {
			if err := i.applyAll(ctx, rec.ID, batch, &report); err != nil {
				return err
			}
		}
		sortErrors(&report)
		if ok, err := i.log.Finish(ctx, rec.ID, domain.ImportPreview, report); err != nil || !ok {
			return cmp.Or(err, errors.New("catalog: the import changed while it was being applied"))
		}
		return nil
	})
	if err != nil {
		return ImportReport{}, fmt.Errorf("catalog: add import not applied: %w", err)
	}
	i.record(ctx, report)
	return report, nil
}

func (i *Importer) prepare(req ImportRequest) (ImportReport, []importRow) {
	report := ImportReport{Mode: req.Mode, File: req.File, Records: len(req.Rows), Errors: []RowError{}}
	valid := i.validate(req.Rows, &report)
	slices.SortFunc(valid, func(a, b importRow) int { return cmp.Compare(a.sku, b.sku) })
	return report, valid
}

func (i *Importer) applyAll(ctx context.Context, id string, batch []importRow, report *ImportReport) error {
	products := make([]domain.Product, len(batch))
	for index, row := range batch {
		products[index] = row.product
	}
	outcomes, err := i.store.Upsert(ctx, domain.ImportAdd, products, id)
	if err != nil {
		return err
	}
	for _, row := range batch {
		count(report, row, outcomes[row.sku])
	}
	return nil
}

func (i *Importer) close(ctx context.Context, report *ImportReport) {
	sortErrors(report)
	i.record(ctx, *report)
}

func sortErrors(report *ImportReport) {
	slices.SortFunc(report.Errors, func(a, b RowError) int {
		return cmp.Or(cmp.Compare(a.Line, b.Line), cmp.Compare(a.Field, b.Field), cmp.Compare(a.Code, b.Code))
	})
}

func (i *Importer) record(ctx context.Context, report ImportReport) {

	for outcome, n := range map[string]int{
		string(OutcomeCreated):   report.Created,
		string(OutcomeUpdated):   report.Updated,
		string(OutcomeUnchanged): report.Unchanged,
		outcomeSkipped:           report.Skipped,
		outcomeRejected:          report.Rejected,
	} {
		i.rec.RecordRows(ctx, outcome, n)
	}
}

func (i *Importer) validate(rows []ImportRow, report *ImportReport) []importRow {
	candidates := make([]importRow, 0, len(rows))
	bySKU := map[string][]int{}
	for _, row := range rows {
		if row.Malformed != "" {
			report.Rejected++
			report.Errors = append(report.Errors, RowError{
				Line: row.Line, Field: rowField, Code: CodeWrongFieldCount, Message: row.Malformed,
			})
			continue
		}
		if row.Empty {
			report.Skipped++
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

func (i *Importer) apply(ctx context.Context, id string, batch []importRow, report *ImportReport) error {
	products := make([]domain.Product, len(batch))
	for index, row := range batch {
		products[index] = row.product
	}
	outcomes, err := i.store.Upsert(ctx, domain.ImportReplace, products, id)
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
		outcomes, err := i.store.Upsert(ctx, domain.ImportReplace, []domain.Product{row.product}, id)
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
	case OutcomeBelowReserved:
		report.Rejected++
		report.Errors = append(report.Errors, RowError{
			Line: row.line, SKU: row.sku, Field: "stock", Code: CodeBelowReserved,
			Message: "must be at least the units reserved by pending purchases",
		})
	default:
		report.Unchanged++
	}
}

func newImportRow(row ImportRow) importRow {
	product, err := domain.NewProduct(row.Product)
	return importRow{
		line:    row.Line,
		sku:     domain.NormalizeSKU(row.Product.SKU),
		product: product,
		errs:    rowErrors(row.Format, err),
	}
}

func rowErrors(format validation.Errors, domain error) validation.Errors {
	var errs validation.Errors
	errors.As(validation.Merge(format, domain), &errs)
	return errs
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
