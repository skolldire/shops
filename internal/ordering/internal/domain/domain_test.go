package domain_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/validation"
)

const (
	key      = "6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"
	productA = "11111111-1111-4111-8111-111111111111"
	productB = "22222222-2222-4222-8222-222222222222"
)

func price(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func input(items ...domain.CheckoutItemInput) domain.CheckoutInput {
	return domain.CheckoutInput{IdempotencyKey: key, Items: items, PaymentToken: "pm_approved"}
}

func item(id string, qty int, p string) domain.CheckoutItemInput {
	return domain.CheckoutItemInput{ProductID: id, Quantity: qty, ExpectedUnitPrice: price(p)}
}

func fieldCodes(t *testing.T, err error) map[string]string {
	t.Helper()
	var errs validation.Errors
	require.ErrorAs(t, err, &errs)
	out := map[string]string{}
	for _, fe := range errs {
		require.NotEmpty(t, fe.Message)
		out[fe.Field] = fe.Code
	}
	return out
}

func TestParseID(t *testing.T) {
	id, err := domain.ParseID("6F1C1D3E-2A4B-4C5D-8E9F-0A1B2C3D4E5F")
	require.NoError(t, err)
	require.Equal(t, domain.ID(key), id)

	id, err = domain.ParseID(key)
	require.NoError(t, err)
	require.Equal(t, domain.ID(key), id)

	for _, raw := range []string{
		"", " " + key, key + " ", "urn:uuid:" + key, "{" + key + "}", strings.ReplaceAll(key, "-", ""),
		"6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5", "6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5g", "6f1c1d3e2a4b-4c5d-8e9f-0a1b2c3d4e5f-",
	} {
		_, err := domain.ParseID(raw)
		require.ErrorIs(t, err, domain.ErrInvalidID, raw)
	}
}

func TestNewCheckoutRequestNormalizesAndTotals(t *testing.T) {
	req, err := domain.NewCheckoutRequest("alice", input(item(strings.ToUpper(productB), 2, "29.9"), item(productA, 3, "0.10")))
	require.NoError(t, err)
	require.Equal(t, "alice", req.UserID)
	require.Equal(t, domain.ID(key), req.IdempotencyKey)
	require.Equal(t, "USD", req.Currency)
	require.Equal(t, []domain.ID{productA, productB}, []domain.ID{req.Items[0].ProductID, req.Items[1].ProductID})
	require.Equal(t, 1, req.Items[0].Index)
	require.Equal(t, 0, req.Items[1].Index)
	require.Equal(t, "60.10", req.Total.StringFixed(2))
	require.Len(t, req.Hash, 64)
}

func TestRequestHashIsCanonical(t *testing.T) {
	hash := func(in domain.CheckoutInput) string {
		t.Helper()
		req, err := domain.NewCheckoutRequest("alice", in)
		require.NoError(t, err)
		return req.Hash
	}
	base := hash(input(item(productA, 2, "29.90"), item(productB, 1, "5")))
	require.Equal(t, base, hash(input(item(productB, 1, "5.00"), item(productA, 2, "29.9"))), "item order and price scale do not matter")
	require.Equal(t, base, hash(input(item(strings.ToUpper(productA), 2, "29.90"), item(productB, 1, "5.0"))), "uuid case does not matter")

	changed := []domain.CheckoutInput{
		input(item(productA, 3, "29.90"), item(productB, 1, "5")),
		input(item(productA, 2, "29.91"), item(productB, 1, "5")),
		input(item(productA, 2, "29.90")),
		{IdempotencyKey: key, Items: []domain.CheckoutItemInput{item(productA, 2, "29.90"), item(productB, 1, "5")}, PaymentToken: "pm_declined"},
	}
	for _, in := range changed {
		require.NotEqual(t, base, hash(in))
	}
	again, err := domain.NewCheckoutRequest("bob", input(item(productA, 2, "29.90"), item(productB, 1, "5")))
	require.NoError(t, err)
	require.Equal(t, base, again.Hash, "the hash covers the request content, the user scopes the key")

	ambiguous := hash(domain.CheckoutInput{IdempotencyKey: key, Items: []domain.CheckoutItemInput{item(productA, 1, "1")}, PaymentToken: "1:a"})
	require.NotEqual(t, ambiguous, hash(domain.CheckoutInput{IdempotencyKey: key, Items: []domain.CheckoutItemInput{item(productA, 1, "1")}, PaymentToken: "1:a1:"}))
}

func TestNewCheckoutRequestValidation(t *testing.T) {
	many := make([]domain.CheckoutItemInput, 51)
	for i := range many {
		many[i] = item(strings.Replace(productA, "1111-4111", "1111-4"+strings.Repeat("0", 3-len(itoa(i)))+itoa(i), 1), 1, "1")
	}
	tests := map[string]struct {
		in   domain.CheckoutInput
		want map[string]string
	}{
		"no items":   {input(), map[string]string{"items": validation.CodeRequired}},
		"too many":   {input(many...), map[string]string{"items": validation.CodeOutOfRange}},
		"bad id":     {input(item("nope", 1, "1")), map[string]string{"items[0].product_id": validation.CodeInvalidFormat}},
		"urn id":     {input(item("urn:uuid:"+productA, 1, "1")), map[string]string{"items[0].product_id": validation.CodeInvalidFormat}},
		"duplicate":  {input(item(productA, 1, "1"), item(strings.ToUpper(productA), 2, "1")), map[string]string{"items[1].product_id": domain.CodeDuplicate}},
		"quantity 0": {input(item(productA, 0, "1")), map[string]string{"items[0].quantity": validation.CodeOutOfRange}},
		"quantity":   {input(item(productA, 100, "1")), map[string]string{"items[0].quantity": validation.CodeOutOfRange}},
		"no price": {input(domain.CheckoutItemInput{ProductID: productA, Quantity: 1}),
			map[string]string{"items[0].expected_unit_price": validation.CodeRequired}},
		"zero price":     {input(item(productA, 1, "0")), map[string]string{"items[0].expected_unit_price": validation.CodeNotPositive}},
		"negative price": {input(item(productA, 1, "-1")), map[string]string{"items[0].expected_unit_price": validation.CodeNotPositive}},
		"decimals":       {input(item(productA, 1, "1.001")), map[string]string{"items[0].expected_unit_price": validation.CodeTooManyDecimals}},
		"no token": {domain.CheckoutInput{IdempotencyKey: key, Items: []domain.CheckoutItemInput{item(productA, 1, "1")}},
			map[string]string{"payment_token": validation.CodeRequired}},
		"long token": {domain.CheckoutInput{IdempotencyKey: key, Items: []domain.CheckoutItemInput{item(productA, 1, "1")}, PaymentToken: strings.Repeat("x", 65)},
			map[string]string{"payment_token": validation.CodeTooLong}},
		"total": {input(item(productA, 2, "9999999999.99")), map[string]string{"total": validation.CodeOutOfRange}},
		"several": {domain.CheckoutInput{IdempotencyKey: key, Items: []domain.CheckoutItemInput{item("x", 0, "0")}},
			map[string]string{"items[0].product_id": validation.CodeInvalidFormat, "items[0].quantity": validation.CodeOutOfRange,
				"items[0].expected_unit_price": validation.CodeNotPositive, "payment_token": validation.CodeRequired}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := domain.NewCheckoutRequest("alice", tc.in)
			require.Equal(t, tc.want, fieldCodes(t, err))
		})
	}

	ok := domain.CheckoutInput{IdempotencyKey: key, Items: []domain.CheckoutItemInput{item(productA, 99, "1")}, PaymentToken: strings.Repeat("x", 64)}
	_, err := domain.NewCheckoutRequest("alice", ok)
	require.NoError(t, err)
}

func itoa(i int) string { return decimal.NewFromInt(int64(i)).String() }

func TestNewCheckoutRequestRejectsTheKeyAndUser(t *testing.T) {
	for _, k := range []string{"", "not-a-uuid", "{" + key + "}"} {
		in := input(item(productA, 1, "1"))
		in.IdempotencyKey = k
		_, err := domain.NewCheckoutRequest("alice", in)
		require.ErrorIs(t, err, domain.ErrInvalidIdempotencyKey)
	}
	_, err := domain.NewCheckoutRequest("", input(item(productA, 1, "1")))
	require.ErrorIs(t, err, domain.ErrUserRequired)
}

func TestSnapshotComparesPrices(t *testing.T) {
	req, err := domain.NewCheckoutRequest("alice", input(item(productB, 2, "29.99"), item(productA, 1, "5.00")))
	require.NoError(t, err)
	items, err := domain.Snapshot(req, []domain.ReservedLine{
		{ProductID: productA, SKU: "A-1", Name: "Alpha", Price: *price("5"), Quantity: 1},
		{ProductID: productB, SKU: "B-1", Name: "Beta", Price: *price("29.99"), Quantity: 2},
	})
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "A-1", items[0].SKU)
	require.Equal(t, "59.98", items[1].LineTotal.StringFixed(2))

	_, err = domain.Snapshot(req, []domain.ReservedLine{
		{ProductID: productA, Price: *price("5.00"), Quantity: 1},
		{ProductID: productB, Price: *price("31.50"), Quantity: 2},
	})
	var changed *domain.PriceChangedError
	require.ErrorAs(t, err, &changed)
	require.Equal(t, validation.Errors{{Field: "items[0].expected_unit_price", Code: domain.CodePriceChanged, Message: "the current price is 31.50"}}, changed.Errors)

	_, err = domain.Snapshot(req, []domain.ReservedLine{{ProductID: productA, Price: *price("5"), Quantity: 1}})
	require.ErrorIs(t, err, domain.ErrReservationMismatch)
}

func TestSnapshotLineTotalsAreExactToTheCent(t *testing.T) {
	cases := []struct {
		price    string
		quantity int
		line     string
	}{
		{"0.10", 3, "0.3"},
		{"1.15", 7, "8.05"},
		{"0.07", 99, "6.93"},
		{"33.33", 3, "99.99"},
		{"19.99", 99, "1979.01"},
		{"0.01", 1, "0.01"},
		{"1234567.89", 99, "122222221.11"},
	}
	inputs := make([]domain.CheckoutItemInput, len(cases))
	lines := make([]domain.ReservedLine, len(cases))
	want := map[domain.ID]string{}
	sum := decimal.Zero
	for i, c := range cases {
		id := fmt.Sprintf("%08d-0000-4000-8000-000000000000", i+1)
		inputs[i] = item(id, c.quantity, c.price)
		lines[i] = domain.ReservedLine{ProductID: domain.ID(id), Price: *price(c.price), Quantity: c.quantity}
		want[domain.ID(id)] = c.line
	}
	req, err := domain.NewCheckoutRequest("alice", input(inputs...))
	require.NoError(t, err)

	items, err := domain.Snapshot(req, lines)
	require.NoError(t, err)
	require.Len(t, items, len(cases))
	for _, it := range items {
		require.Equal(t, want[it.ProductID], it.LineTotal.String(), "no rounding at any step")
		require.GreaterOrEqual(t, it.LineTotal.Exponent(), int32(-2), "at most 2 decimals")
		sum = sum.Add(it.LineTotal)
	}
	require.True(t, req.Total.Equal(sum), "the total is the exact sum of the line totals")
	require.Equal(t, "122224315.40", req.Total.StringFixed(2))
}

func TestPurchaseTransitions(t *testing.T) {
	p := domain.Purchase{Status: domain.StatusPending}
	require.False(t, p.Finished())
	require.NoError(t, p.Apply(domain.Confirmed("fake_1")))
	require.Equal(t, domain.StatusConfirmed, p.Status)
	require.Equal(t, "fake_1", p.PaymentRef)
	require.True(t, p.Finished())
	require.ErrorIs(t, p.Apply(domain.Failed("card_declined")), domain.ErrInvalidTransition)

	p = domain.Purchase{Status: domain.StatusPending}
	require.NoError(t, p.Apply(domain.Failed("card_declined")))
	require.Equal(t, "card_declined", p.FailureReason)
	require.ErrorIs(t, p.Apply(domain.Confirmed("x")), domain.ErrInvalidTransition)

	p = domain.Purchase{Status: domain.StatusPending}
	require.ErrorIs(t, p.Apply(domain.Outcome{Status: domain.StatusPending}), domain.ErrInvalidTransition)
}

func TestNewPendingPurchase(t *testing.T) {
	req, err := domain.NewCheckoutRequest("alice", input(item(productA, 2, "1.25")))
	require.NoError(t, err)
	p := domain.NewPendingPurchase(req)
	require.Equal(t, domain.StatusPending, p.Status)
	require.Equal(t, "2.50", p.Total.StringFixed(2))
	require.Equal(t, "pm_approved", p.PaymentToken)
	require.Equal(t, req.Hash, p.RequestHash)
	require.Equal(t, []domain.Quantity{{ProductID: productA, Quantity: 2}}, req.Quantities())
}

func TestCheckQuantityAndPage(t *testing.T) {
	require.NoError(t, domain.CheckQuantity(1))
	require.NoError(t, domain.CheckQuantity(99))
	require.Equal(t, map[string]string{"quantity": validation.CodeOutOfRange}, fieldCodes(t, domain.CheckQuantity(0)))
	require.Equal(t, map[string]string{"quantity": validation.CodeOutOfRange}, fieldCodes(t, domain.CheckQuantity(100)))

	p, err := domain.NewPage(nil, nil)
	require.NoError(t, err)
	require.Equal(t, domain.Page{Page: 1, PageSize: 20}, p, "an absent page and size take the defaults")
	p, err = domain.NewPage(intp(3), intp(10))
	require.NoError(t, err)
	require.Equal(t, 20, p.Offset())
	p, err = domain.NewPage(intp(100000), intp(100))
	require.NoError(t, err)
	require.Equal(t, domain.Page{Page: 100000, PageSize: 100}, p)
	for name, values := range map[string][2]*int{
		"explicit zero":   {intp(0), intp(0)},
		"negative":        {intp(-1), intp(-1)},
		"above the limit": {intp(100001), intp(101)},
	} {
		require.Equal(t, map[string]string{"page": validation.CodeOutOfRange, "page_size": validation.CodeOutOfRange}, fieldCodes(t, func() error {
			_, err := domain.NewPage(values[0], values[1])
			return err
		}()), name)
	}
}

func intp(n int) *int {
	return &n
}
