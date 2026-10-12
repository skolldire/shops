package validation_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/validation"
)

func fieldErrors(pairs ...string) validation.Errors {
	var errs validation.Errors
	for i := 0; i+1 < len(pairs); i += 2 {
		errs.Add(pairs[i], pairs[i+1], pairs[i]+" "+pairs[i+1])
	}
	return errs
}

func TestMerge(t *testing.T) {
	tests := map[string]struct {
		format validation.Errors
		domain error
		want   validation.Errors
	}{
		"nothing to report":       {nil, nil, nil},
		"only format errors":      {fieldErrors("price", "invalid_format"), nil, fieldErrors("price", "invalid_format")},
		"only domain errors":      {nil, fieldErrors("name", "required").Err(), fieldErrors("name", "required")},
		"different fields":        {fieldErrors("price", "invalid_format"), fieldErrors("name", "required").Err(), fieldErrors("price", "invalid_format", "name", "required")},
		"format wins on a field":  {fieldErrors("price", "invalid_format"), fieldErrors("price", "required", "name", "required").Err(), fieldErrors("price", "invalid_format", "name", "required")},
		"wrapped domain errors":   {nil, fmt.Errorf("catalog: %w", fieldErrors("name", "required").Err()), fieldErrors("name", "required")},
		"domain order is kept":    {fieldErrors("stock", "invalid_format"), fieldErrors("sku", "required", "name", "required").Err(), fieldErrors("stock", "invalid_format", "sku", "required", "name", "required")},
		"several format per name": {fieldErrors("price", "invalid_format", "stock", "out_of_range"), fieldErrors("stock", "required").Err(), fieldErrors("price", "invalid_format", "stock", "out_of_range")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := validation.Merge(tt.format, tt.domain)

			if tt.want == nil {
				require.NoError(t, err)
				return
			}
			var got validation.Errors
			require.ErrorAs(t, err, &got)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestMergeReturnsOtherErrorsUntouched(t *testing.T) {
	boom := errors.New("boom")

	err := validation.Merge(fieldErrors("price", "invalid_format"), boom)

	require.ErrorIs(t, err, boom)
	var fieldErrs validation.Errors
	require.False(t, errors.As(err, &fieldErrs))
}

func TestMergeDoesNotChangeItsInputs(t *testing.T) {
	format := fieldErrors("price", "invalid_format")
	domain := fieldErrors("name", "required")

	_ = validation.Merge(format, domain.Err())

	require.Equal(t, fieldErrors("price", "invalid_format"), format)
	require.Equal(t, fieldErrors("name", "required"), domain)
}
