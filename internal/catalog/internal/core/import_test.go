package core_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/platform/logger"
)

const csvHeader = "name,sku,description,category,price,stock,weight_kg\n"

type fakeStore struct {
	existing  map[string]core.Product
	deleted   map[string]bool
	failBatch bool
	failSKU   map[string]bool
	batches   [][]string
	onBatch   func(applied int)
}

func (f *fakeStore) Upsert(ctx context.Context, products []core.Product) (map[string]core.UpsertOutcome, error) {
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
	out := make(map[string]core.UpsertOutcome, len(products))
	for _, p := range products {
		current, found := f.existing[p.SKU]
		switch {
		case f.failSKU[p.SKU]:
			return nil, errors.New("row exploded: host=10.0.0.5")
		case f.deleted[p.SKU]:
			out[p.SKU] = core.OutcomeDeleted
		case !found:
			out[p.SKU] = core.OutcomeCreated
		case current.Name == p.Name && current.Stock == p.Stock && current.Price.Equal(p.Price):
			out[p.SKU] = core.OutcomeUnchanged
		default:
			out[p.SKU] = core.OutcomeUpdated
		}
		if f.existing == nil {
			f.existing = map[string]core.Product{}
		}
		if !f.deleted[p.SKU] {
			f.existing[p.SKU] = p
		}
	}
	return out, nil
}

type fakeRecorder struct {
	rows      map[string]int
	durations int
}

func (f *fakeRecorder) RecordRows(_ context.Context, outcome string, n int) {
	if f.rows == nil {
		f.rows = map[string]int{}
	}
	f.rows[outcome] += n
}

func (f *fakeRecorder) RecordDuration(context.Context, time.Duration) { f.durations++ }

func newImporter(t *testing.T, store *fakeStore, rec *fakeRecorder, limits core.ImportLimits) (*core.Importer, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log, err := logger.New(logger.Config{Level: "info"}, &logs)
	require.NoError(t, err)
	importer, err := core.NewImporter(store, rec, log, limits)
	require.NoError(t, err)
	return importer, &logs
}

var roomy = core.ImportLimits{MaxBytes: 1 << 20, MaxRows: 1000}

func run(t *testing.T, store *fakeStore, body string) (core.ImportReport, *fakeRecorder) {
	t.Helper()
	rec := &fakeRecorder{}
	importer, _ := newImporter(t, store, rec, roomy)
	report, err := importer.Import(context.Background(), strings.NewReader(body), "products.csv")
	require.NoError(t, err)
	return report, rec
}

func codes(report core.ImportReport) []string {
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
		if e.Code == core.CodeDuplicateInFile {
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
		existing: map[string]core.Product{
			"SAME-1": product(t, "SAME-1", "Same", "5", 1),
			"CHG-1":  product(t, "CHG-1", "Old name", "5", 1),
		},
		deleted: map[string]bool{"GONE-1": true},
	}
	body := csvHeader +
		"New,NEW-1,,A,5,1,1\n" +
		"Same,SAME-1,,A,5,1,1\n" +
		"New name,CHG-1,,A,5,1,1\n" +
		"Gone,GONE-1,,A,5,1,1\n" +
		",,,,,,\n" +
		"Bad,BAD-1,,A,free,1,1\n"

	report, rec := run(t, store, body)

	require.Equal(t, 6, report.Records)
	require.Equal(t, 1, report.Skipped)
	require.Equal(t, 1, report.Created)
	require.Equal(t, 1, report.Updated)
	require.Equal(t, 1, report.Unchanged)
	require.Equal(t, 2, report.Rejected)
	require.Equal(t, report.Records, report.Skipped+report.Created+report.Updated+report.Unchanged+report.Rejected)
	require.Equal(t, []string{"5 GONE-1 sku sku_deleted", "7 BAD-1 price invalid_format"}, codes(report))
	require.Equal(t, map[string]int{"created": 1, "updated": 1, "unchanged": 1, "skipped": 1, "rejected": 2}, rec.rows)
	require.Equal(t, 1, rec.durations)
}

func TestImportRecordsEveryOutcomeEvenWhenItIsZero(t *testing.T) {
	_, rec := run(t, &fakeStore{}, csvHeader)

	require.Equal(t, map[string]int{"created": 0, "updated": 0, "unchanged": 0, "skipped": 0, "rejected": 0}, rec.rows)
}

func TestImportDescribesTheFile(t *testing.T) {
	body := csvHeader + "Shoes,RS-1,,Sports,9.99,1,0.3\n"
	sum := sha256.Sum256([]byte(body))

	report, _ := run(t, &fakeStore{}, body)

	require.Equal(t, core.ImportFile{Name: "products.csv", Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, report.File)
}

func TestImportAppliesSortedBatchesOfFiveHundred(t *testing.T) {
	var body strings.Builder
	body.WriteString(csvHeader)
	for i := 1200; i > 0; i-- {
		fmt.Fprintf(&body, "Item,SKU-%04d,,A,5,1,1\n", i)
	}
	store := &fakeStore{}
	rec := &fakeRecorder{}
	importer, _ := newImporter(t, store, rec, core.ImportLimits{MaxBytes: 1 << 20, MaxRows: 2000})

	report, err := importer.Import(context.Background(), strings.NewReader(body.String()), "big.csv")

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
	importer, logs := newImporter(t, store, rec, roomy)
	body := csvHeader + "A,A-1,,A,5,1,1\nB,B-1,,A,5,1,1\nC,C-1,,A,5,1,1\n"

	report, err := importer.Import(context.Background(), strings.NewReader(body), "products.csv")

	require.NoError(t, err)
	require.Equal(t, 2, report.Created)
	require.Equal(t, 1, report.Rejected)
	require.Equal(t, []string{"3 B-1 row rejected_by_database"}, codes(report))
	require.NotContains(t, report.Errors[0].Message, "10.0.0.5")
	require.Contains(t, logs.String(), "row exploded")
	require.Equal(t, [][]string{{"A-1", "B-1", "C-1"}, {"A-1"}, {"B-1"}, {"C-1"}}, store.batches)
}

func TestImportStopsWhenTheContextIsCancelled(t *testing.T) {
	store := &fakeStore{failBatch: true}
	importer, _ := newImporter(t, store, &fakeRecorder{}, roomy)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := importer.Import(ctx, strings.NewReader(csvHeader+"A,A-1,,A,5,1,1\nB,B-1,,A,5,1,1\n"), "products.csv")

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
	importer, _ := newImporter(t, store, rec, core.ImportLimits{MaxBytes: 1 << 20, MaxRows: 2000})

	report, err := importer.Import(ctx, strings.NewReader(body.String()), "big.csv")

	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, store.batches, 3)
	require.Equal(t, 1000, report.Created, "the two committed batches are still reported")
	require.Equal(t, 1201, report.Records)
	require.Equal(t, 1, report.Rejected)
	require.Equal(t, []string{"1202 BAD-1 price invalid_format"}, codes(report))
	require.Equal(t, "big.csv", report.File.Name)
	require.Len(t, report.File.SHA256, 64)
	require.Equal(t, 1000, rec.rows["created"])
	require.Equal(t, 1, rec.durations)
}

func TestImportRejectsTheWholeFile(t *testing.T) {
	limits := core.ImportLimits{MaxBytes: 200, MaxRows: 3}
	row := "A,A-1,,A,5,1,1\n"
	tests := map[string]struct {
		body   string
		target error
	}{
		"another header":   {"sku,name\n" + row, core.ErrInvalidHeader},
		"empty file":       {"", core.ErrInvalidHeader},
		"broken quotes":    {csvHeader + "A,\"A-1,,A,5,1,1\n", core.ErrInvalidCSV},
		"invalid utf-8":    {csvHeader + "A\xff,A-1,,A,5,1,1\n", core.ErrInvalidCSV},
		"too many rows":    {csvHeader + strings.Repeat(",,,,,,\n", 4), core.ErrTooManyRows},
		"too many bytes":   {csvHeader + row + strings.Repeat(" ", 200), core.ErrFileTooLarge},
		"rows before size": {csvHeader + strings.Repeat(row, 4), core.ErrTooManyRows},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			rec := &fakeRecorder{}
			importer, _ := newImporter(t, store, rec, limits)

			report, err := importer.Import(context.Background(), strings.NewReader(tt.body), "products.csv")

			require.ErrorIs(t, err, tt.target)
			require.Equal(t, core.ImportReport{}, report)
			require.Empty(t, store.batches, "nothing is applied when the file is rejected")
			require.Empty(t, rec.rows)
		})
	}
}

func TestImportReportsTheLineOfAMalformedFile(t *testing.T) {
	importer, _ := newImporter(t, &fakeStore{}, &fakeRecorder{}, roomy)

	_, err := importer.Import(context.Background(), strings.NewReader(csvHeader+"A,A-1,,A,5,1,1\nB,\"B-1,,A,5,1,1\n"), "products.csv")

	var invalid *core.InvalidCSVError
	require.ErrorAs(t, err, &invalid)
	require.Equal(t, 3, invalid.Line)
	require.NotEmpty(t, invalid.Reason)
}

func TestImportAcceptsAFileOfExactlyTheByteLimit(t *testing.T) {
	body := csvHeader + "A,A-1,,A,5,1,1\n"
	importer, _ := newImporter(t, &fakeStore{}, &fakeRecorder{}, core.ImportLimits{MaxBytes: int64(len(body)), MaxRows: 10})

	report, err := importer.Import(context.Background(), strings.NewReader(body), "products.csv")

	require.NoError(t, err)
	require.Equal(t, 1, report.Created)
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestImportReturnsReadFailures(t *testing.T) {
	importer, _ := newImporter(t, &fakeStore{}, &fakeRecorder{}, roomy)

	_, err := importer.Import(context.Background(), brokenReader{}, "products.csv")

	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestNewImporterRequiresItsDependencies(t *testing.T) {
	log, err := logger.New(logger.Config{Level: "info"}, io.Discard)
	require.NoError(t, err)

	_, err = core.NewImporter(nil, &fakeRecorder{}, log, roomy)
	require.EqualError(t, err, "catalog: importer requires a product store")
	_, err = core.NewImporter(&fakeStore{}, nil, log, roomy)
	require.EqualError(t, err, "catalog: importer requires a recorder")
	_, err = core.NewImporter(&fakeStore{}, &fakeRecorder{}, nil, roomy)
	require.EqualError(t, err, "catalog: importer requires a logger")
	_, err = core.NewImporter(&fakeStore{}, &fakeRecorder{}, log, core.ImportLimits{})
	require.EqualError(t, err, "catalog: importer requires positive limits")
}

func product(t *testing.T, sku, name, price string, stock int) core.Product {
	t.Helper()
	d, err := core.ParseDecimal(price)
	require.NoError(t, err)
	return core.Product{SKU: sku, Name: name, Price: d, Stock: stock}
}
