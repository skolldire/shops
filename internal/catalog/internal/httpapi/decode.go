package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/platform/validation"
)

var (
	errInvalidRequest  = errors.New("request body is not a valid JSON object with the expected fields")
	errRequestTooLarge = errors.New("request body is too large")
	null               = []byte("null")
)

type productBody struct {
	SKU         json.RawMessage `json:"sku"`
	Name        json.RawMessage `json:"name"`
	Description json.RawMessage `json:"description"`
	Category    json.RawMessage `json:"category"`
	Price       json.RawMessage `json:"price"`
	Stock       json.RawMessage `json:"stock"`
	WeightKg    json.RawMessage `json:"weight_kg"`
}

func decodeBody(r *http.Request) (productBody, error) {
	var body productBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(&body)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errInvalidRequest
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return productBody{}, errRequestTooLarge
	case err != nil:
		return productBody{}, errInvalidRequest
	}
	return body, nil
}

func (b productBody) createInput() (core.NewProductInput, validation.Errors) {
	var errs validation.Errors
	in := core.NewProductInput{
		SKU:         stringValue(&errs, "sku", b.SKU),
		Name:        stringValue(&errs, "name", b.Name),
		Description: stringValue(&errs, "description", b.Description),
		Category:    stringValue(&errs, "category", b.Category),
		Price:       decimalField(&errs, "price", b.Price),
		Stock:       intField(&errs, "stock", b.Stock),
		WeightKg:    decimalField(&errs, "weight_kg", b.WeightKg),
	}
	return in, errs
}

func (b productBody) changes() (core.Changes, validation.Errors) {
	var errs validation.Errors
	c := core.Changes{
		Name:        stringField(&errs, "name", b.Name),
		Description: stringField(&errs, "description", b.Description),
		Category:    stringField(&errs, "category", b.Category),
		Price:       decimalField(&errs, "price", b.Price),
		Stock:       intField(&errs, "stock", b.Stock),
		WeightKg:    decimalField(&errs, "weight_kg", b.WeightKg),
	}
	if b.SKU != nil {
		sku := ""
		c.SKU = &sku
	}
	return c, errs
}

func stringValue(errs *validation.Errors, field string, raw json.RawMessage) string {
	if s := stringField(errs, field, raw); s != nil {
		return *s
	}
	return ""
}

func stringField(errs *validation.Errors, field string, raw json.RawMessage) *string {
	if raw == nil {
		return nil
	}
	var s string
	if bytes.Equal(raw, null) || json.Unmarshal(raw, &s) != nil {
		errs.Add(field, validation.CodeInvalidFormat, "must be a string")
		return nil
	}
	return &s
}

func decimalField(errs *validation.Errors, field string, raw json.RawMessage) *decimal.Decimal {
	if raw == nil {
		return nil
	}
	literal := string(raw)
	if raw[0] == '"' {
		if json.Unmarshal(raw, &literal) != nil {
			literal = ""
		}
	}
	d, err := core.ParseDecimal(literal)
	if bytes.Equal(raw, null) || err != nil {
		errs.Add(field, validation.CodeInvalidFormat, core.ErrInvalidDecimal.Error())
		return nil
	}
	return &d
}

func intField(errs *validation.Errors, field string, raw json.RawMessage) *int64 {
	if raw == nil {
		return nil
	}
	n, err := strconv.ParseInt(string(raw), 10, 32)
	var numErr *strconv.NumError
	switch {
	case errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange):
		errs.Add(field, validation.CodeOutOfRange, "must be between 0 and 2147483647")
		return nil
	case err != nil:
		errs.Add(field, validation.CodeInvalidFormat, "must be an integer")
		return nil
	}
	return &n
}

func merge(format validation.Errors, domain error) error {
	var domainErrs validation.Errors
	if !errors.As(domain, &domainErrs) {
		return format.Err()
	}
	seen := make(map[string]bool, len(format))
	for _, fe := range format {
		seen[fe.Field] = true
	}
	merged := append(validation.Errors{}, format...)
	for _, fe := range domainErrs {
		if !seen[fe.Field] {
			merged = append(merged, fe)
		}
	}
	return merged.Err()
}
