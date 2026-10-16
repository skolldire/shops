package domain_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/validation"
)

func TestNewPaging(t *testing.T) {
	p, err := domain.NewPaging(0, 0)
	require.NoError(t, err)
	require.Equal(t, domain.Paging{Page: 1, PageSize: 20}, p)
	require.Equal(t, 0, p.Offset())

	p, err = domain.NewPaging(3, 100)
	require.NoError(t, err)
	require.Equal(t, 200, p.Offset())

	_, err = domain.NewPaging(100001, 101)
	var errs validation.Errors
	require.ErrorAs(t, err, &errs)
	require.Len(t, errs, 2)
	require.Equal(t, "page", errs[0].Field)
	require.Equal(t, "page_size", errs[1].Field)
	require.Equal(t, validation.CodeOutOfRange, errs[1].Code)
}
