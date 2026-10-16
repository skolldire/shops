package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/validation"
)

var (
	errInvalidRequest  = errors.New("request body is not a valid JSON object with the expected fields")
	errRequestTooLarge = errors.New("request body is too large")
	null               = []byte("null")
)

type itemBody struct {
	ProductID         json.RawMessage `json:"product_id"`
	Quantity          json.RawMessage `json:"quantity"`
	ExpectedUnitPrice json.RawMessage `json:"expected_unit_price"`
}

type checkoutBody struct {
	Items        []itemBody      `json:"items"`
	PaymentToken json.RawMessage `json:"payment_token"`
}

func decodeCheckout(r *http.Request) (checkoutBody, error) {
	var body checkoutBody
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&body)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errInvalidRequest
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return checkoutBody{}, errRequestTooLarge
	case err != nil:
		return checkoutBody{}, errInvalidRequest
	}
	return body, nil
}

func (b checkoutBody) input(key string) (domain.CheckoutInput, validation.Errors) {
	var errs validation.Errors
	in := domain.CheckoutInput{
		IdempotencyKey: key,
		PaymentToken:   stringField(&errs, "payment_token", b.PaymentToken),
		Items:          make([]domain.CheckoutItemInput, len(b.Items)),
	}
	for i, it := range b.Items {
		in.Items[i] = domain.CheckoutItemInput{
			ProductID:         stringField(&errs, domain.ItemField(i, "product_id"), it.ProductID),
			Quantity:          quantityField(&errs, domain.ItemField(i, "quantity"), it.Quantity),
			ExpectedUnitPrice: priceField(&errs, domain.ItemField(i, "expected_unit_price"), it.ExpectedUnitPrice),
		}
	}
	return in, errs
}

func stringField(errs *validation.Errors, field string, raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if bytes.Equal(raw, null) || json.Unmarshal(raw, &s) != nil {
		errs.Add(field, validation.CodeInvalidFormat, "must be a string")
		return ""
	}
	return s
}

func quantityField(errs *validation.Errors, field string, raw json.RawMessage) int {
	if raw == nil {
		return 0
	}
	n, err := catalog.ParseWholeNumber(string(raw))
	if bytes.Equal(raw, null) || err != nil {
		errs.Add(field, validation.CodeInvalidFormat, "must be a whole number such as 2")
		return 0
	}
	return int(n)
}

func priceField(errs *validation.Errors, field string, raw json.RawMessage) *decimal.Decimal {
	if raw == nil {
		return nil
	}
	literal := string(raw)
	if raw[0] == '"' && json.Unmarshal(raw, &literal) != nil {
		literal = ""
	}
	d, err := catalog.ParseDecimal(literal)
	if bytes.Equal(raw, null) || err != nil {
		errs.Add(field, validation.CodeInvalidFormat, "must be a plain decimal number such as 29.99")
		return nil
	}
	return &d
}
