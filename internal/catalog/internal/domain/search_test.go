package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

func TestNewSearchDefaults(t *testing.T) {
	s, err := domain.NewSearch(domain.SearchInput{})

	require.NoError(t, err)
	require.Equal(t, domain.Search{Sort: domain.SortName, Page: 1, PageSize: 20}, s)
	require.Equal(t, 0, s.Offset())
}

func TestNewSearchValues(t *testing.T) {
	s, err := domain.NewSearch(domain.SearchInput{
		Q:        "  50% off  ",
		Category: " Home   Audio ",
		MinPrice: dec("10"),
		MaxPrice: dec("99.50"),
		InStock:  true,
		Sort:     "-price",
		Page:     3,
		PageSize: 25,
	})

	require.NoError(t, err)
	require.Equal(t, "50% off", s.Q)
	require.Equal(t, "Home Audio", s.Category)
	require.Equal(t, "10", s.MinPrice.String())
	require.Equal(t, "99.5", s.MaxPrice.String())
	require.True(t, s.InStock)
	require.Equal(t, domain.SortPrice, s.Sort)
	require.True(t, s.Descending)
	require.Equal(t, 50, s.Offset())
}

func TestNewSearchSortWhitelist(t *testing.T) {
	for raw, want := range map[string]struct {
		field domain.SortField
		desc  bool
	}{
		"name": {domain.SortName, false}, "-name": {domain.SortName, true},
		"price": {domain.SortPrice, false}, "created_at": {domain.SortCreatedAt, false},
		"-created_at": {domain.SortCreatedAt, true},
	} {
		s, err := domain.NewSearch(domain.SearchInput{Sort: raw})
		require.NoError(t, err, raw)
		require.Equal(t, want.field, s.Sort, raw)
		require.Equal(t, want.desc, s.Descending, raw)
	}
}

func TestNewSearchErrors(t *testing.T) {
	tests := map[string]struct {
		in    domain.SearchInput
		field string
		code  string
	}{
		"q too long":          {domain.SearchInput{Q: strings.Repeat("q", 101)}, "q", "too_long"},
		"min price negative":  {domain.SearchInput{MinPrice: dec("-0.01")}, "min_price", "out_of_range"},
		"max price negative":  {domain.SearchInput{MaxPrice: dec("-1")}, "max_price", "out_of_range"},
		"min above max":       {domain.SearchInput{MinPrice: dec("50"), MaxPrice: dec("10")}, "min_price", "out_of_range"},
		"sort not allowed":    {domain.SearchInput{Sort: "stock"}, "sort", "invalid_format"},
		"sort sql injection":  {domain.SearchInput{Sort: "name; DROP TABLE products"}, "sort", "invalid_format"},
		"page negative":       {domain.SearchInput{Page: -1}, "page", "out_of_range"},
		"page above maximum":  {domain.SearchInput{Page: 100001}, "page", "out_of_range"},
		"page size negative":  {domain.SearchInput{PageSize: -1}, "page_size", "out_of_range"},
		"page size above 100": {domain.SearchInput{PageSize: 101}, "page_size", "out_of_range"},
		"min price too large": {domain.SearchInput{MinPrice: dec("10000000000")}, "min_price", "out_of_range"},
		"max price too large": {domain.SearchInput{MaxPrice: dec("10000000000")}, "max_price", "out_of_range"},
		"huge min above max":  {domain.SearchInput{MinPrice: dec("100000000000"), MaxPrice: dec("10")}, "min_price", "out_of_range"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s, err := domain.NewSearch(tt.in)

			require.Equal(t, map[string]string{tt.field: tt.code}, fieldCodes(t, err))
			require.Equal(t, domain.Search{}, s)
		})
	}
}

func TestNewSearchReportsEveryError(t *testing.T) {
	_, err := domain.NewSearch(domain.SearchInput{Page: -1, PageSize: 500, Sort: "rating", Q: strings.Repeat("q", 101)})

	require.Equal(t, map[string]string{
		"page": "out_of_range", "page_size": "out_of_range", "sort": "invalid_format", "q": "too_long",
	}, fieldCodes(t, err))
}

func TestNewSearchUsesDefaultsForZeroValues(t *testing.T) {
	s, err := domain.NewSearch(domain.SearchInput{})

	require.NoError(t, err)
	require.Equal(t, 1, s.Page)
	require.Equal(t, 20, s.PageSize)

	_, err = domain.NewSearch(domain.SearchInput{Page: -1, PageSize: 101})
	require.Equal(t, map[string]string{"page": "out_of_range", "page_size": "out_of_range"}, fieldCodes(t, err))

	_, err = domain.NewSearch(domain.SearchInput{Page: 100001})
	require.Equal(t, map[string]string{"page": "out_of_range"}, fieldCodes(t, err))

	s, err = domain.NewSearch(domain.SearchInput{Page: 100000, PageSize: 100})
	require.NoError(t, err)
	require.Equal(t, 9999900, s.Offset())
}
