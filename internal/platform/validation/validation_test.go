package validation_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/validation"
)

func TestErrNilWhenEmpty(t *testing.T) {
	var errs validation.Errors

	require.NoError(t, errs.Err())
	require.Nil(t, errs.Err())
}

func TestAddAccumulatesInOrder(t *testing.T) {
	var errs validation.Errors
	errs.Add("sku", validation.CodeRequired, "is required")
	errs.Add("price", validation.CodeNotPositive, "must be greater than 0")

	err := errs.Err()

	require.EqualError(t, err, "validation failed: sku: is required; price: must be greater than 0")
	var got validation.Errors
	require.True(t, errors.As(err, &got))
	require.Equal(t, validation.Errors{
		{Field: "sku", Code: "required", Message: "is required"},
		{Field: "price", Code: "not_positive", Message: "must be greater than 0"},
	}, got)
}

func TestErrorsSurviveWrapping(t *testing.T) {
	var errs validation.Errors
	errs.Add("name", validation.CodeTooLong, "must be at most 200 characters")

	wrapped := errors.Join(errors.New("catalog: create product"), errs.Err())

	var got validation.Errors
	require.ErrorAs(t, wrapped, &got)
	require.Len(t, got, 1)
}
