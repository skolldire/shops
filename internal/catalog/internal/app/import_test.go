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
	onBatch    func(applied int)
}

func (f *fakeStore) Upsert(ctx context.Context, _ domain.ImportMode, products []domain.Product, reference string) (map[string]app.UpsertOutcome, error) {
	f.references = append(f.references, reference)
	skus := make([]string, len(products))
	for i, p := range products {
		skus[i] = p.SKU
	}
	f.batches = append(f.batches, skus)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.onBatch != nil {
		defer f.onBatch(len(f.batches))
	}
	if f.failBatch && len(products) > 1 {
		return nil, errors.New("batch exploded: host=10.0.0.5")
	}
	out := make(map[string]app.UpsertOutcome, len(products))
	for _, p := range products {
		current, found := f.existing[p.SKU]
		switch {
		case f.failSKU[p.SKU]:
			return nil, errors.New("row exploded: host=10.0.0.5")
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
	return out, nil
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
	importer, err := app.NewImporter(store, rec, failures)
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
	report, err := importer.Import(context.Background(), productsFile, rowsOf(body))
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

	report, err := importer.Import(context.Background(), productsFile, nil)

	require.NoError(t, err)
	require.Equal(t, app.ImportReport{File: productsFile, Errors: []app.RowError{}}, report)
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

	report, err := importer.Import(context.Background(), app.ImportFile{Name: "big.csv"}, rowsOf(body.String()))

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

	report, err := importer.Import(context.Background(), app.ImportFile{Name: "products.csv"}, rowsOf(body))

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

	_, err := importer.Import(ctx, app.ImportFile{Name: "products.csv"}, rowsOf(csvHeader+"A,A-1,,A,5,1,1\nB,B-1,,A,5,1,1\n"))

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

	report, err := importer.Import(ctx, app.ImportFile{Name: "big.csv"}, rowsOf(body.String()))

	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, store.batches, 3)
	require.Equal(t, 1000, report.Created, "the two committed batches are still reported")
	require.Equal(t, 1201, report.Records)
	require.Equal(t, 1, report.Rejected)
	require.Equal(t, []string{"1202 BAD-1 price invalid_format"}, codes(report))
	require.Equal(t, "big.csv", report.File.Name)
	require.Equal(t, 1000, rec.rows["created"])
}

func TestNewImporterRequiresItsDependencies(t *testing.T) {
	_, err := app.NewImporter(nil, &fakeRecorder{}, &fakeFailures{})
	require.EqualError(t, err, "catalog: importer requires a product store")
	_, err = app.NewImporter(&fakeStore{}, nil, &fakeFailures{})
	require.EqualError(t, err, "catalog: importer requires a recorder")
	_, err = app.NewImporter(&fakeStore{}, &fakeRecorder{}, nil)
	require.EqualError(t, err, "catalog: importer requires a failure reporter")
}

func product(t *testing.T, sku, name, price string, stock int) domain.Product {
	t.Helper()
	d, err := domain.ParseDecimal(price)
	require.NoError(t, err)
	return domain.Product{SKU: sku, Name: name, Price: d, Stock: stock}
}
