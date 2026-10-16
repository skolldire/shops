package ordering

import (
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/logger"
	"github.com/skolldire/shops/internal/platform/telemetry"
	"github.com/skolldire/shops/internal/platform/validation"
)

func TestPublicErrors(t *testing.T) {
	fields := validation.Errors{{Field: "items[0].quantity", Code: "insufficient_stock", Message: "only 1 available"}}
	tests := []struct {
		in   error
		want error
	}{
		{domain.ErrInvalidIdempotencyKey, ErrInvalidIdempotencyKey},
		{domain.ErrInvalidID, ErrInvalidID},
		{app.ErrIdempotencyConflict, ErrIdempotencyConflict},
		{app.ErrPurchaseNotFound, ErrPurchaseNotFound},
		{&app.UnavailableProductError{ProductID: "x"}, ErrProductUnavailable},
		{app.ErrCartFull, ErrCartFull},
		{errors.Join(app.ErrBusy, errors.New("40P01")), ErrTryAgain},
		{&app.RejectedError{Reason: app.ErrInsufficientStock, Errors: fields}, ErrInsufficientStock},
		{&app.RejectedError{Reason: app.ErrPriceChanged, Errors: fields}, ErrPriceChanged},
		{&app.RejectedError{Reason: app.ErrProductUnavailable, Errors: fields}, ErrProductUnavailable},
	}
	for _, tc := range tests {
		require.ErrorIs(t, publicError(tc.in), tc.want, tc.in.Error())
	}
	var rejected *RejectedError
	require.ErrorAs(t, publicError(&app.RejectedError{Reason: app.ErrInsufficientStock, Errors: fields}), &rejected)
	require.Equal(t, fields, rejected.Errors)

	var errs validation.Errors
	require.ErrorAs(t, publicError(fields), &errs)
	require.NoError(t, publicError(nil))
	other := errors.New("boom")
	require.Equal(t, other, publicError(other))
}

func TestNewModuleRequiresItsDependencies(t *testing.T) {
	_, err := NewModule(Deps{})
	require.Error(t, err)

	log, err := logger.New(logger.Config{Level: "error"}, io.Discard)
	require.NoError(t, err)
	_, err = newModule(ports{}, Deps{Log: log, Metrics: telemetry.Noop()})
	require.Error(t, err)
}

func TestThePublicCartFullErrorNamesTheSharedLimit(t *testing.T) {
	require.Equal(t, MaxCartLines, domain.MaxItems, "the cart and the checkout share one limit")
	require.Equal(t, "ordering: the cart already has "+strconv.Itoa(MaxCartLines)+" lines", ErrCartFull.Error())
	require.Equal(t, app.ErrCartFull.Error(), ErrCartFull.Error())
}
