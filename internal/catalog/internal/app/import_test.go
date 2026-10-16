package app_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/app/apptest"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/validation"
)

const csvHeader = "name,sku,description,category,price,stock,weight_kg\n"

type fakeStore struct {
	existing   map[string]domain.Product
	deleted    map[string]bool
	reserved   map[string]int
	failBatch  bool
	failSKU    map[string]bool
	batches    [][]string
	references []string
	modes      []domain.ImportMode
	onBatch    func(applied int)
	calls      []string
	locked     [][]string
}

func (f *fakeStore) LockExisting(_ context.Context, skus []string) error {
	f.calls = append(f.calls, "lock")
	f.locked = append(f.locked, skus)
	return nil
}

func (f *fakeStore) Upsert(ctx context.Context, mode domain.ImportMode, products []domain.Product, reference string) (app.UpsertResult, error) {
	f.calls = append(f.calls, "upsert")
	f.references = append(f.references, reference)
	f.modes = append(f.modes, mode)
	skus := make([]string, len(products))
	for i, p := range products {
		skus[i] = p.SKU
	}
	f.batches = append(f.batches, skus)
	if err := ctx.Err(); err != nil {
		return app.UpsertResult{}, err
	}
	if f.onBatch != nil {
		defer f.onBatch(len(f.batches))
	}
	if f.failBatch && len(products) > 1 {
		return app.UpsertResult{}, errors.New("batch exploded: host=10.0.0.5")
	}
	out := make(map[string]app.UpsertOutcome, len(products))
	for _, p := range products {
		current, found := f.existing[p.SKU]
		switch {
		case f.failSKU[p.SKU]:
			return app.UpsertResult{}, errors.New("row exploded: host=10.0.0.5")
		case f.deleted[p.SKU]:
			out[p.SKU] = app.OutcomeDeleted
		case p.Stock < f.reserved[p.SKU]:
			out[p.SKU] = app.OutcomeBelowReserved
			continue
		case !found:
			out[p.SKU] = app.OutcomeCreated
		case current.Name == p.Name && current.Stock == p.Stock && current.Price.Equal(p.Price):
			out[p.SKU] = app.OutcomeUnchanged
		default:
			out[p.SKU] = app.OutcomeUpdated
		}
		if f.existing == nil {
			f.existing = map[string]domain.Product{}
		}
		if !f.deleted[p.SKU] {
			f.existing[p.SKU] = p
		}
	}
	return app.UpsertResult{Outcomes: out}, nil
}

type fakeRecorder struct {
	rows map[string]int
}

func (f *fakeRecorder) RecordRows(_ context.Context, outcome string, n int) {
	if f.rows == nil {
		f.rows = map[string]int{}
	}
	f.rows[outcome] += n
}

type fakeFailures struct {
	batches []string
	rows    []string
}

func (f *fakeFailures) BatchRetried(_ context.Context, rows int, cause error) {
	f.batches = append(f.batches, fmt.Sprintf("%d rows: %v", rows, cause))
}

func (f *fakeFailures) RowRejected(_ context.Context, line int, sku string, cause error) {
	f.rows = append(f.rows, fmt.Sprintf("%d %s: %v", line, sku, cause))
}

func newImporter(t *testing.T, store *fakeStore, rec *fakeRecorder) (*app.Importer, *fakeFailures) {
	t.Helper()
	failures := &fakeFailures{}
	importer, err := app.NewImporter(store, apptest.NewLog(), apptest.Tx{}, rec, failures)
	require.NoError(t, err)
	return importer, failures
}

func rowsOf(body string) []app.ImportRow {
	lines := strings.Split(strings.TrimSuffix(strings.TrimPrefix(body, csvHeader), "\n"), "\n")
	rows := make([]app.ImportRow, 0, len(lines))
	for i, line := range lines {
		if line == "" && len(lines) == 1 {
			break
		}
		rows = append(rows, rowOf(i+2, strings.Split(line, ",")))
	}
	return rows
}

func rowOf(line int, f []string) app.ImportRow {
	blank := true
	for _, value := range f {
		blank = blank && strings.TrimSpace(value) == ""
	}
	switch {
	case blank:
		return app.ImportRow{Line: line, Empty: true}
	case len(f) != 7:
		return app.ImportRow{Line: line, Malformed: fmt.Sprintf("must have 7 fields, found %d", len(f))}
	}
	row := app.ImportRow{Line: line, Product: domain.NewProductInput{Name: f[0], SKU: f[1], Description: f[2], Category: f[3]}}
	row.Product.Price = decimalOf(&row.Format, "price", f[4])
	row.Product.WeightKg = decimalOf(&row.Format, "weight_kg", f[6])
	if stock := strings.TrimSpace(f[5]); stock != "" {
		n, err := domain.ParseWholeNumber(stock)
		if err != nil {
			row.Format.Add("stock", domain.WholeNumberErrorCode(err), err.Error())
		} else {
			row.Product.Stock = &n
		}
	}
	return row
}

func decimalOf(errs *validation.Errors, field, raw string) *decimal.Decimal {
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

var productsFile = app.ImportFile{Name: "products.csv", Bytes: 8930, SHA256: "6fc46b84"}

func run(t *testing.T, store *fakeStore, body string) (app.ImportReport, *fakeRecorder) {
	t.Helper()
	rec := &fakeRecorder{}
	importer, _ := newImporter(t, store, rec)
	report, err := importer.Import(context.Background(), app.ImportRequest{File: productsFile, Rows: rowsOf(body), Mode: domain.ImportReplace})
	require.NoError(t, err)
	return report, rec
}

func codes(report app.ImportReport) []string {
	out := make([]string, len(report.Errors))
	for i, e := range report.Errors {
		out[i] = fmt.Sprintf("%d %s %s %s", e.Line, e.SKU, e.Field, e.Code)
	}
	return out
}

func TestImportRowRules(t *testing.T) {
	tests := map[string]struct {
		row  string
		want []string
	}{
		"valid row":               {"Shoes,RS-1,Light,Sports,9.99,1,0.3", nil},
		"valid with spaces":       {"  Shoes , rs-1 ,, Sports , 9.99 , 1 , 0.3 ", nil},
		"valid with zero stock":   {"Shoes,RS-1,,Sports,0.01,0,0", nil},
		"missing name":            {",RS-1,,Sports,9.99,1,0.3", []string{"2 RS-1 name required"}},
		"blank name":              {"   ,RS-1,,Sports,9.99,1,0.3", []string{"2 RS-1 name required"}},
		"missing sku":             {"Shoes,,,Sports,9.99,1,0.3", []string{"2  sku required"}},
		"sku with invalid chars":  {"Shoes,RS 1,,Sports,9.99,1,0.3", []string{"2 RS 1 sku invalid_format"}},
		"missing category":        {"Shoes,RS-1,,,9.99,1,0.3", []string{"2 RS-1 category required"}},
		"missing price":           {"Shoes,RS-1,,Sports,,1,0.3", []string{"2 RS-1 price required"}},
		"price with currency":     {"Shoes,RS-1,,Sports,$9.99,1,0.3", []string{"2 RS-1 price invalid_format"}},
		"price as a word":         {"Shoes,RS-1,,Sports,free,1,0.3", []string{"2 RS-1 price invalid_format"}},
		"price with exponent":     {"Shoes,RS-1,,Sports,1e100000000,1,0.3", []string{"2 RS-1 price invalid_format"}},
		"price zero":              {"Shoes,RS-1,,Sports,0.00,1,0.3", []string{"2 RS-1 price not_positive"}},
		"price negative":          {"Shoes,RS-1,,Sports,-1,1,0.3", []string{"2 RS-1 price not_positive"}},
		"price three decimals":    {"Shoes,RS-1,,Sports,9.999,1,0.3", []string{"2 RS-1 price too_many_decimals"}},
		"price too large":         {"Shoes,RS-1,,Sports,10000000000,1,0.3", []string{"2 RS-1 price out_of_range"}},
		"missing stock":           {"Shoes,RS-1,,Sports,9.99,,0.3", []string{"2 RS-1 stock required"}},
		"stock negative":          {"Shoes,RS-1,,Sports,9.99,-5,0.3", []string{"2 RS-1 stock out_of_range"}},
		"stock above int32":       {"Shoes,RS-1,,Sports,9.99,2147483648,0.3", []string{"2 RS-1 stock out_of_range"}},
		"stock beyond int64":      {"Shoes,RS-1,,Sports,9.99,99999999999999999999,0.3", []string{"2 RS-1 stock out_of_range"}},
		"stock with decimals":     {"Shoes,RS-1,,Sports,9.99,1.0,0.3", []string{"2 RS-1 stock invalid_format"}},
		"stock with plus sign":    {"Shoes,RS-1,,Sports,9.99,+5,0.3", []string{"2 RS-1 stock invalid_format"}},
		"stock with exponent":     {"Shoes,RS-1,,Sports,9.99,1e3,0.3", []string{"2 RS-1 stock invalid_format"}},
		"missing weight":          {"Shoes,RS-1,,Sports,9.99,1,", []string{"2 RS-1 weight_kg required"}},
		"weight negative":         {"Shoes,RS-1,,Sports,9.99,1,-0.1", []string{"2 RS-1 weight_kg out_of_range"}},
		"weight four decimals":    {"Shoes,RS-1,,Sports,9.99,1,0.0001", []string{"2 RS-1 weight_kg too_many_decimals"}},
		"weight as a word":        {"Shoes,RS-1,,Sports,9.99,1,heavy", []string{"2 RS-1 weight_kg invalid_format"}},
		"too few fields":          {"Shoes,RS-1", []string{"2  row wrong_field_count"}},
		"too many fields":         {"Shoes,RS-1,,Sports,9.99,1,0.3,extra", []string{"2  row wrong_field_count"}},
		"several errors, ordered": {",bad sku,,,free,-1,", []string{"2 BAD SKU category required", "2 BAD SKU name required", "2 BAD SKU price invalid_format", "2 BAD SKU sku invalid_format", "2 BAD SKU stock out_of_range", "2 BAD SKU weight_kg required"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}

			report, _ := run(t, store, csvHeader+tt.row+"\n")

			require.Equal(t, 1, report.Records)
			if tt.want == nil {
				require.Empty(t, report.Errors)
				require.NotNil(t, report.Errors)
				require.Equal(t, 1, report.Created)
				require.Zero(t, report.Rejected)
				return
			}
			require.Equal(t, tt.want, codes(report))
			require.Equal(t, 1, report.Rejected, "a row counts once however many errors it has")
			require.Zero(t, report.Created)
			require.Empty(t, store.batches, "rejected rows never reach the store")
			for _, e := range report.Errors {
				require.NotEmpty(t, e.Message)
			}
		})
	}
}

func TestImportNormalizesValuesBeforeStoringThem(t *testing.T) {
	store := &fakeStore{}

	run(t, store, csvHeader+"  Shoes , rs-1 , Light ,  Trail   Running , 9.90 , 7 , 0.300 \n")

	p := store.existing["RS-1"]
	require.Equal(t, "Shoes", p.Name)
	require.Equal(t, "Light", p.Description)
	require.Equal(t, "Trail Running", p.Category)
	require.Equal(t, "9.9", p.Price.String())
	require.Equal(t, 7, p.Stock)
	require.Equal(t, "0.3", p.WeightKg.String())
}

func TestImportSkipsEmptyRows(t *testing.T) {
	store := &fakeStore{}

	report, rec := run(t, store, csvHeader+",,,,,,\nShoes,RS-1,,Sports,9.99,1,0.3\n  , ,\t,,,,\n")

	require.Equal(t, 3, report.Records)
	require.Equal(t, 2, report.Skipped)
	require.Equal(t, 1, report.Created)
	require.Empty(t, report.Errors)
	require.Equal(t, 2, rec.rows["skipped"])
}

func TestImportRejectsEverySKURepeatedInTheFile(t *testing.T) {
	store := &fakeStore{}
	body := csvHeader +
		"Shoes,rs-001,,Sports,9.99,1,0.3\n" +
		"Mug,MG-1,,Home,2,0,0\n" +
		"Boots, RS-001 ,,Sports,19.99,1,0.9\n" +
		"Speaker,BS-1,,Audio,5,1,1\n" +
		",bs-1,,Audio,5,1,1\n" +
		"Speaker 3,BS-1,,Audio,5,1,1\n" +
		"Short,BS-1\n"

	report, _ := run(t, store, body)

	require.Equal(t, []string{
		"2 RS-001 sku duplicate_in_file",
		"4 RS-001 sku duplicate_in_file",
		"5 BS-1 sku duplicate_in_file",
		"6 BS-1 name required",
		"6 BS-1 sku duplicate_in_file",
		"7 BS-1 sku duplicate_in_file",
		"8  row wrong_field_count",
	}, codes(report))
	messages := map[int]string{}
	for _, e := range report.Errors {
		if e.Code == app.CodeDuplicateInFile {
			messages[e.Line] = e.Message
		}
	}
	require.Equal(t, map[int]string{
		2: "also used on line 4",
		4: "also used on line 2",
		5: "also used on lines 6 and 7",
		6: "also used on lines 5 and 7",
		7: "also used on lines 5 and 6",
	}, messages)
	require.Equal(t, 6, report.Rejected)
	require.Equal(t, 1, report.Created)
	require.Equal(t, [][]string{{"MG-1"}}, store.batches)
}

func TestImportCountsEveryOutcome(t *testing.T) {
	store := &fakeStore{
		existing: map[string]domain.Product{
			"SAME-1": product(t, "SAME-1", "Same", "5", 1),
			"CHG-1":  product(t, "CHG-1", "Old name", "5", 1),
		},
		deleted:  map[string]bool{"GONE-1": true},
		reserved: map[string]int{"HELD-1": 3},
	}
	body := csvHeader +
		"New,NEW-1,,A,5,1,1\n" +
		"Same,SAME-1,,A,5,1,1\n" +
		"New name,CHG-1,,A,5,1,1\n" +
		"Gone,GONE-1,,A,5,1,1\n" +
		",,,,,,\n" +
		"Bad,BAD-1,,A,free,1,1\n" +
		"Held,HELD-1,,A,5,2,1\n"

	report, rec := run(t, store, body)

	require.Equal(t, 7, report.Records)
	require.Equal(t, 1, report.Skipped)
	require.Equal(t, 1, report.Created)
	require.Equal(t, 1, report.Updated)
	require.Equal(t, 1, report.Unchanged)
	require.Equal(t, 3, report.Rejected)
	require.Equal(t, report.Records, report.Skipped+report.Created+report.Updated+report.Unchanged+report.Rejected)
	require.Equal(t, []string{"5 GONE-1 sku sku_deleted", "7 BAD-1 price invalid_format", "8 HELD-1 stock below_reserved"}, codes(report))
	require.Equal(t, "must be at least the units reserved by pending purchases", report.Errors[2].Message)
	require.Equal(t, map[string]int{"created": 1, "updated": 1, "unchanged": 1, "skipped": 1, "rejected": 3}, rec.rows)
}

func TestImportRecordsEveryOutcomeEvenWhenItIsZero(t *testing.T) {
	_, rec := run(t, &fakeStore{}, csvHeader)

	require.Equal(t, map[string]int{"created": 0, "updated": 0, "unchanged": 0, "skipped": 0, "rejected": 0}, rec.rows)
}

func TestImportReportsTheFileItWasGiven(t *testing.T) {
	report, _ := run(t, &fakeStore{}, csvHeader+"Shoes,RS-1,,Sports,9.99,1,0.3\n")

	require.Equal(t, productsFile, report.File)
}

func TestImportOfNoRowsIsAnEmptyReport(t *testing.T) {
	importer, _ := newImporter(t, &fakeStore{}, &fakeRecorder{})

	report, err := importer.Import(context.Background(), app.ImportRequest{File: productsFile, Rows: nil, Mode: domain.ImportReplace})

	require.NoError(t, err)
	require.Equal(t, app.ImportReport{ID: report.ID, Mode: domain.ImportReplace, File: productsFile, Errors: []app.RowError{}, Changes: []domain.ImportChange{}}, report)
	require.NotEmpty(t, report.ID)
}

func TestImportAppliesSortedBatchesOfFiveHundred(t *testing.T) {
	var body strings.Builder
	body.WriteString(csvHeader)
	for i := 1200; i > 0; i-- {
		fmt.Fprintf(&body, "Item,SKU-%04d,,A,5,1,1\n", i)
	}
	store := &fakeStore{}
	rec := &fakeRecorder{}
	importer, _ := newImporter(t, store, rec)

	report, err := importer.Import(context.Background(), app.ImportRequest{File: app.ImportFile{Name: "big.csv"}, Rows: rowsOf(body.String()), Mode: domain.ImportReplace})

	require.NoError(t, err)
	require.Equal(t, 1200, report.Created)
	require.Len(t, store.batches, 3)
	require.Len(t, store.batches[0], 500)
	require.Len(t, store.batches[1], 500)
	require.Len(t, store.batches[2], 200)
	var all []string
	for _, batch := range store.batches {
		all = append(all, batch...)
	}
	require.True(t, sort.StringsAreSorted(all), "rows are applied in sku order")
}

func TestImportRetriesAFailedBatchRowByRow(t *testing.T) {
	store := &fakeStore{failBatch: true, failSKU: map[string]bool{"B-1": true}}
	rec := &fakeRecorder{}
	importer, failures := newImporter(t, store, rec)
	body := csvHeader + "A,A-1,,A,5,1,1\nB,B-1,,A,5,1,1\nC,C-1,,A,5,1,1\n"

	report, err := importer.Import(context.Background(), app.ImportRequest{File: app.ImportFile{Name: "products.csv"}, Rows: rowsOf(body), Mode: domain.ImportReplace})

	require.NoError(t, err)
	require.Equal(t, 2, report.Created)
	require.Equal(t, 1, report.Rejected)
	require.Equal(t, []string{"3 B-1 row rejected_by_database"}, codes(report))
	require.NotContains(t, report.Errors[0].Message, "10.0.0.5")
	require.Equal(t, []string{"3 rows: batch exploded: host=10.0.0.5"}, failures.batches, "the retry is reported once with its cause")
	require.Equal(t, []string{"3 B-1: row exploded: host=10.0.0.5"}, failures.rows, "the cause goes to the reporter, not to the report")
	require.Equal(t, [][]string{{"A-1", "B-1", "C-1"}, {"A-1"}, {"B-1"}, {"C-1"}}, store.batches)
	require.Equal(t, map[string]int{"created": 2, "updated": 0, "unchanged": 0, "skipped": 0, "rejected": 1}, rec.rows,
		"each row is counted once although its batch was tried again row by row")
}

func TestImportStopsWhenTheContextIsCancelled(t *testing.T) {
	store := &fakeStore{failBatch: true}
	importer, _ := newImporter(t, store, &fakeRecorder{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := importer.Import(ctx, app.ImportRequest{File: app.ImportFile{Name: "products.csv"}, Rows: rowsOf(csvHeader + "A,A-1,,A,5,1,1\nB,B-1,,A,5,1,1\n"), Mode: domain.ImportReplace})

	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, store.batches, 1)
}

func TestImportInterruptedKeepsTheCountsOfTheAppliedBatches(t *testing.T) {
	var body strings.Builder
	body.WriteString(csvHeader)
	for i := 1; i <= 1200; i++ {
		fmt.Fprintf(&body, "Item,SKU-%04d,,A,5,1,1\n", i)
	}
	body.WriteString("Bad,BAD-1,,A,free,1,1\n")
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeStore{onBatch: func(applied int) {
		if applied == 2 {
			cancel()
		}
	}}
	rec := &fakeRecorder{}
	importer, _ := newImporter(t, store, rec)

	report, err := importer.Import(ctx, app.ImportRequest{File: app.ImportFile{Name: "big.csv"}, Rows: rowsOf(body.String()), Mode: domain.ImportReplace})

	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, store.batches, 3)
	require.Equal(t, 1000, report.Created, "the two committed batches are still reported")
	require.Equal(t, 1201, report.Records)
	require.Equal(t, 1, report.Rejected)
	require.Equal(t, []string{"1202 BAD-1 price invalid_format"}, codes(report))
	require.Equal(t, "big.csv", report.File.Name)
	require.Equal(t, 1000, rec.rows["created"])
}

func TestAnAddImportIsAppliedInOneTransactionAndFinishedThere(t *testing.T) {
	store, log := &fakeStore{}, apptest.NewLog()
	importer, err := app.NewImporter(store, log, apptest.Tx{}, &fakeRecorder{}, &fakeFailures{})
	require.NoError(t, err)
	body := csvHeader + "A,A-1,,A,5,1,1\nB,B-1,,A,5,2,1\nBad,BAD-1,,A,free,1,1\n"

	report, err := importer.Import(context.Background(), app.ImportRequest{File: productsFile, Rows: rowsOf(body), Mode: domain.ImportAdd, Key: importKey})

	require.NoError(t, err)
	require.Equal(t, domain.ImportAdd, report.Mode)
	require.Equal(t, []int{2, 1}, []int{report.Created, report.Rejected})
	require.Equal(t, []domain.ImportMode{domain.ImportAdd}, store.modes)
	require.Equal(t, []string{report.ID}, store.references)
	stored, err := log.Get(context.Background(), report.ID)
	require.NoError(t, err)
	require.Equal(t, domain.ImportApplied, stored.Status)
	require.Equal(t, report, stored.Report)
}

func TestAFailedAddImportAppliesNothingAndIsNotRetriedRowByRow(t *testing.T) {
	store, log, rec := &fakeStore{failBatch: true}, apptest.NewLog(), &fakeRecorder{}
	importer, err := app.NewImporter(store, log, apptest.Tx{}, rec, &fakeFailures{})
	require.NoError(t, err)

	_, err = importer.Import(context.Background(), app.ImportRequest{File: productsFile, Rows: rowsOf(csvHeader + "A,A-1,,A,5,1,1\nB,B-1,,A,5,1,1\n"), Mode: domain.ImportAdd, Key: importKey})

	require.ErrorContains(t, err, "add import not applied")
	require.Len(t, store.batches, 1, "an add import never falls back to row by row")
	require.Empty(t, log.Finished)
	require.Empty(t, rec.rows, "nothing was applied, so nothing is counted")
}

func TestAReplaceImportIsAppliedWhileItIsApplyingAndKeepsItWhenInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeStore{onBatch: func(int) { cancel() }}
	log := apptest.NewLog()
	importer, err := app.NewImporter(store, log, apptest.Tx{}, &fakeRecorder{}, &fakeFailures{})
	require.NoError(t, err)
	var body strings.Builder
	body.WriteString(csvHeader)
	for i := 1; i <= 600; i++ {
		fmt.Fprintf(&body, "Item,SKU-%04d,,A,5,1,1\n", i)
	}

	report, err := importer.Import(ctx, app.ImportRequest{File: productsFile, Rows: rowsOf(body.String()), Mode: domain.ImportReplace})

	require.ErrorIs(t, err, context.Canceled)
	stored, getErr := log.Get(context.Background(), report.ID)
	require.NoError(t, getErr)
	require.Equal(t, domain.ImportApplying, stored.Status, "an interrupted import stays applying")
	require.Empty(t, log.Finished)
}

const (
	importKey = "6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"
	otherKey  = "7f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"
)

func keyed(mode domain.ImportMode, key, hash, body string) app.ImportRequest {
	return app.ImportRequest{File: app.ImportFile{Name: "stock.csv", SHA256: hash}, Rows: rowsOf(body), Mode: mode, Key: key}
}

func TestAnAddImportNeedsAValidKey(t *testing.T) {
	importer, _ := newImporter(t, &fakeStore{}, &fakeRecorder{})
	body := csvHeader + "A,A-1,,A,5,1,1\n"
	_, err := importer.Import(context.Background(), keyed(domain.ImportAdd, "", "h", body))
	require.ErrorIs(t, err, app.ErrIdempotencyKeyRequired)
	_, err = importer.Import(context.Background(), keyed(domain.ImportReplace, "not-a-uuid", "h", body))
	require.ErrorIs(t, err, app.ErrInvalidIdempotencyKey)
}

func TestTheSameKeyAndFileReturnTheOriginalReportWithoutApplyingAgain(t *testing.T) {
	for _, mode := range []domain.ImportMode{domain.ImportAdd, domain.ImportReplace} {
		t.Run(string(mode), func(t *testing.T) {
			store := &fakeStore{}
			importer, _ := newImporter(t, store, &fakeRecorder{})
			body := csvHeader + "A,A-1,,A,5,1,1\n"
			first, err := importer.Import(context.Background(), keyed(mode, importKey, "h", body))
			require.NoError(t, err)

			again, err := importer.Import(context.Background(), keyed(mode, strings.ToUpper(importKey), "h", body))
			require.NoError(t, err)
			require.Equal(t, first, again)
			require.Len(t, store.batches, 1, "the second request applies nothing")
		})
	}
}

func TestTheSameKeyWithAnotherFileOrModeIsAConflict(t *testing.T) {
	importer, _ := newImporter(t, &fakeStore{}, &fakeRecorder{})
	body := csvHeader + "A,A-1,,A,5,1,1\n"
	_, err := importer.Import(context.Background(), keyed(domain.ImportAdd, importKey, "h", body))
	require.NoError(t, err)
	_, err = importer.Import(context.Background(), keyed(domain.ImportAdd, importKey, "other", body))
	require.ErrorIs(t, err, app.ErrIdempotencyConflict)
	_, err = importer.Import(context.Background(), keyed(domain.ImportReplace, importKey, "h", body))
	require.ErrorIs(t, err, app.ErrIdempotencyConflict)
}

func TestARetriedReplaceResumesAnImportLeftApplying(t *testing.T) {
	store, log := &fakeStore{}, apptest.NewLog()
	importer, err := app.NewImporter(store, log, apptest.Tx{}, &fakeRecorder{}, &fakeFailures{})
	require.NoError(t, err)
	body := csvHeader + "A,A-1,,A,5,1,1\nB,B-1,,A,5,1,1\n"
	log.Set(app.ImportRecord{ID: "00000000-0000-4000-8000-000000000099", Key: importKey, Mode: domain.ImportReplace,
		Status: domain.ImportApplying, File: app.ImportFile{Name: "stock.csv", SHA256: "h"}})

	report, err := importer.Import(context.Background(), keyed(domain.ImportReplace, otherKey, "h", body))
	require.NoError(t, err)
	require.NotEqual(t, "00000000-0000-4000-8000-000000000099", report.ID, "another key is another import")

	report, err = importer.Import(context.Background(), keyed(domain.ImportReplace, importKey, "h", body))
	require.NoError(t, err)
	require.Equal(t, "00000000-0000-4000-8000-000000000099", report.ID)
	require.Equal(t, 2, report.Unchanged, "the whole file is applied again from the start")
	require.Len(t, store.batches, 2)
	stored, err := log.Get(context.Background(), report.ID)
	require.NoError(t, err)
	require.Equal(t, domain.ImportApplied, stored.Status)
}

func TestAddingTheSameFileAgainNeedsAnExplicitConfirmation(t *testing.T) {
	store := &fakeStore{}
	importer, _ := newImporter(t, store, &fakeRecorder{})
	body := csvHeader + "A,A-1,,A,5,1,1\n"
	_, err := importer.Import(context.Background(), keyed(domain.ImportAdd, importKey, "h", body))
	require.NoError(t, err)

	_, err = importer.Import(context.Background(), keyed(domain.ImportAdd, otherKey, "h", body))
	var duplicate *app.DuplicateFileError
	require.ErrorAs(t, err, &duplicate)
	require.ErrorIs(t, err, app.ErrDuplicateFile)
	require.Len(t, store.batches, 1, "nothing is applied without the confirmation")

	confirmed := keyed(domain.ImportAdd, "9f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f", "h", body)
	confirmed.AllowDuplicate = true
	_, err = importer.Import(context.Background(), confirmed)
	require.NoError(t, err)
	require.Len(t, store.batches, 2)

	_, err = importer.Import(context.Background(), keyed(domain.ImportReplace, "8f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f", "h", body))
	require.NoError(t, err, "replacing with the same file again is harmless and needs no confirmation")
}

func TestTheReportListsTheExistingProductsBeforeAndAfter(t *testing.T) {
	old := app.UpsertResult{
		Outcomes: map[string]app.UpsertOutcome{"A-1": app.OutcomeUpdated, "B-1": app.OutcomeUnchanged, "N-1": app.OutcomeCreated, "G-1": app.OutcomeDeleted},
		Changes: map[string]domain.ImportChange{
			"A-1": {SKU: "A-1", StockBefore: 50, StockAfter: 60, PriceBefore: decimal.RequireFromString("29.99"), PriceAfter: decimal.RequireFromString("39.99")},
			"B-1": {SKU: "B-1", StockBefore: 3, StockAfter: 3, PriceBefore: decimal.RequireFromString("1"), PriceAfter: decimal.RequireFromString("1")},
		},
	}
	importer, err := app.NewImporter(fixedStore{old}, apptest.NewLog(), apptest.Tx{}, &fakeRecorder{}, &fakeFailures{})
	require.NoError(t, err)
	body := csvHeader + "A,A-1,,A,39.99,10,1\nB,B-1,,A,1,0,1\nN,N-1,,A,1,1,1\nG,G-1,,A,1,1,1\n"

	report, err := importer.Import(context.Background(), keyed(domain.ImportAdd, importKey, "h", body))

	require.NoError(t, err)
	require.Equal(t, []domain.ImportChange{old.Changes["A-1"], old.Changes["B-1"]}, report.Changes, "only existing products, in sku order")
}

type fixedStore struct{ result app.UpsertResult }

func (f fixedStore) Upsert(context.Context, domain.ImportMode, []domain.Product, string) (app.UpsertResult, error) {
	return f.result, nil
}

func (fixedStore) LockExisting(context.Context, []string) error { return nil }

func TestNewImporterRequiresItsDependencies(t *testing.T) {
	log, tx := apptest.NewLog(), apptest.Tx{}
	_, err := app.NewImporter(nil, log, tx, &fakeRecorder{}, &fakeFailures{})
	require.EqualError(t, err, "catalog: importer requires a product store")
	_, err = app.NewImporter(&fakeStore{}, nil, tx, &fakeRecorder{}, &fakeFailures{})
	require.EqualError(t, err, "catalog: importer requires an import log")
	_, err = app.NewImporter(&fakeStore{}, log, nil, &fakeRecorder{}, &fakeFailures{})
	require.EqualError(t, err, "catalog: importer requires a transaction manager")
	_, err = app.NewImporter(&fakeStore{}, log, tx, nil, &fakeFailures{})
	require.EqualError(t, err, "catalog: importer requires a recorder")
	_, err = app.NewImporter(&fakeStore{}, log, tx, &fakeRecorder{}, nil)
	require.EqualError(t, err, "catalog: importer requires a failure reporter")
}

func product(t *testing.T, sku, name, price string, stock int) domain.Product {
	t.Helper()
	d, err := domain.ParseDecimal(price)
	require.NoError(t, err)
	return domain.Product{SKU: sku, Name: name, Price: d, Stock: stock}
}

func largeFile(rows int) string {
	var body strings.Builder
	body.WriteString(csvHeader)
	for i := rows; i >= 1; i-- {
		fmt.Fprintf(&body, "Item,N-%04d,,A,5,1,1\n", i)
	}
	return body.String()
}

func TestAnAddLocksEveryExistingProductOnceBeforeItsFirstBatch(t *testing.T) {
	store := &fakeStore{}
	importer, _ := newImporter(t, store, &fakeRecorder{})
	_, err := importer.Import(context.Background(), keyed(domain.ImportAdd, importKey, "h", largeFile(600)+"Bad,,,A,5,1,1\n"))
	require.NoError(t, err)

	require.Equal(t, []string{"lock", "upsert", "upsert"}, store.calls, "one lock for the whole file, then the batches")
	require.Len(t, store.locked[0], 600, "every valid row, never a rejected one")
	require.True(t, sort.StringsAreSorted(store.locked[0]))
	require.Equal(t, append(append([]string{}, store.batches[0]...), store.batches[1]...), store.locked[0], "the batches follow the order of the lock")
}

func TestAReplaceLocksNothingBeyondItsBatches(t *testing.T) {
	store := &fakeStore{}
	importer, _ := newImporter(t, store, &fakeRecorder{})
	_, err := importer.Import(context.Background(), keyed(domain.ImportReplace, importKey, "h", largeFile(600)))
	require.NoError(t, err)
	require.Equal(t, []string{"upsert", "upsert"}, store.calls, "each batch commits on its own and locks its rows inside the upsert")
}

func TestTheBatchesInsertTheSKUsInOneAscendingOrder(t *testing.T) {
	for _, mode := range []domain.ImportMode{domain.ImportAdd, domain.ImportReplace} {
		t.Run(string(mode), func(t *testing.T) {
			store := &fakeStore{}
			importer, _ := newImporter(t, store, &fakeRecorder{})
			_, err := importer.Import(context.Background(), keyed(mode, importKey, "h", largeFile(1200)))
			require.NoError(t, err)
			var all []string
			for _, batch := range store.batches {
				all = append(all, batch...)
			}
			require.Len(t, store.batches, 3)
			require.Len(t, all, 1200)
			require.True(t, sort.StringsAreSorted(all), "two imports insert new skus in the same global order")
		})
	}
}

func TestAnImportWhileAnotherRunsIsRefusedWithoutEffects(t *testing.T) {
	for _, mode := range []domain.ImportMode{domain.ImportAdd, domain.ImportReplace} {
		t.Run(string(mode), func(t *testing.T) {
			store, log := &fakeStore{}, apptest.NewLog()
			importer, err := app.NewImporter(store, log, apptest.Tx{}, &fakeRecorder{}, &fakeFailures{})
			require.NoError(t, err)
			log.Busy = true

			_, err = importer.Import(context.Background(), keyed(mode, importKey, "h", csvHeader+"A,A-1,,A,5,1,1\n"))
			require.ErrorIs(t, err, app.ErrImportInProgress)
			require.Empty(t, store.calls, "nothing was locked or applied")
			_, found, err := log.ByKey(context.Background(), importKey)
			require.NoError(t, err)
			require.False(t, found, "no import was recorded")

			log.Busy = false
			report, err := importer.Import(context.Background(), keyed(mode, importKey, "h", csvHeader+"A,A-1,,A,5,1,1\n"))
			require.NoError(t, err)
			require.Equal(t, 1, report.Created, "the retry applies the file")
		})
	}
}

func TestARepeatedKeyIsAnsweredWhileAnotherImportRuns(t *testing.T) {
	store, log := &fakeStore{}, apptest.NewLog()
	importer, err := app.NewImporter(store, log, apptest.Tx{}, &fakeRecorder{}, &fakeFailures{})
	require.NoError(t, err)
	body := csvHeader + "A,A-1,,A,5,1,1\n"
	first, err := importer.Import(context.Background(), keyed(domain.ImportAdd, importKey, "h", body))
	require.NoError(t, err)

	log.Busy = true
	again, err := importer.Import(context.Background(), keyed(domain.ImportAdd, importKey, "h", body))
	require.NoError(t, err)
	require.Equal(t, first, again, "a replay never waits for the lock")
	_, err = importer.Import(context.Background(), keyed(domain.ImportAdd, importKey, "other", body))
	require.ErrorIs(t, err, app.ErrIdempotencyConflict, "a conflict is reported before the lock too")
	require.Len(t, store.batches, 1)
}
