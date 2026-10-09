package core_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/core"
)

func TestParseSearchDefaults(t *testing.T) {
	s, err := core.ParseSearch(url.Values{})

	require.NoError(t, err)
	require.Equal(t, core.Search{Sort: core.SortName, Page: 1, PageSize: 20}, s)
	require.Equal(t, 0, s.Offset())
}

func TestParseSearchValues(t *testing.T) {
	s, err := core.ParseSearch(url.Values{
		"q":         {"  50% off  "},
		"category":  {" Home   Audio "},
		"min_price": {"10"},
		"max_price": {"99.50"},
		"in_stock":  {"true"},
		"sort":      {"-price"},
		"page":      {"3"},
		"page_size": {"25"},
	})

	require.NoError(t, err)
	require.Equal(t, "50% off", s.Q)
	require.Equal(t, "Home Audio", s.Category)
	require.Equal(t, "10", s.MinPrice.String())
	require.Equal(t, "99.5", s.MaxPrice.String())
	require.True(t, s.InStock)
	require.Equal(t, core.SortPrice, s.Sort)
	require.True(t, s.Descending)
	require.Equal(t, 50, s.Offset())
}

func TestParseSearchSortWhitelist(t *testing.T) {
	for raw, want := range map[string]struct {
		field core.SortField
		desc  bool
	}{
		"name": {core.SortName, false}, "-name": {core.SortName, true},
		"price": {core.SortPrice, false}, "created_at": {core.SortCreatedAt, false},
		"-created_at": {core.SortCreatedAt, true},
	} {
		s, err := core.ParseSearch(url.Values{"sort": {raw}})
		require.NoError(t, err, raw)
		require.Equal(t, want.field, s.Sort, raw)
		require.Equal(t, want.desc, s.Descending, raw)
	}
}

func TestParseSearchErrors(t *testing.T) {
	tests := map[string]struct {
		values url.Values
		field  string
		code   string
	}{
		"q too long":          {url.Values{"q": {strings.Repeat("q", 101)}}, "q", "too_long"},
		"min price not a num": {url.Values{"min_price": {"cheap"}}, "min_price", "invalid_format"},
		"max price negative":  {url.Values{"max_price": {"-1"}}, "max_price", "out_of_range"},
		"min above max":       {url.Values{"min_price": {"50"}, "max_price": {"10"}}, "min_price", "out_of_range"},
		"in_stock not bool":   {url.Values{"in_stock": {"maybe"}}, "in_stock", "invalid_format"},
		"sort not allowed":    {url.Values{"sort": {"stock"}}, "sort", "invalid_format"},
		"sort sql injection":  {url.Values{"sort": {"name; DROP TABLE products"}}, "sort", "invalid_format"},
		"page not a number":   {url.Values{"page": {"two"}}, "page", "invalid_format"},
		"page zero":           {url.Values{"page": {"0"}}, "page", "out_of_range"},
		"page size zero":      {url.Values{"page_size": {"0"}}, "page_size", "out_of_range"},
		"page size above 100": {url.Values{"page_size": {"101"}}, "page_size", "out_of_range"},
		"page beyond int64":   {url.Values{"page": {"9223372036854775807"}}, "page", "out_of_range"},
		"page above maximum":  {url.Values{"page": {"100001"}}, "page", "out_of_range"},
		"min price too large": {url.Values{"min_price": {"10000000000"}}, "min_price", "out_of_range"},
		"max price too large": {url.Values{"max_price": {"1e1000"}}, "max_price", "out_of_range"},
		"huge min above max":  {url.Values{"min_price": {"1e20"}, "max_price": {"10"}}, "min_price", "out_of_range"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := core.ParseSearch(tt.values)

			require.Equal(t, map[string]string{tt.field: tt.code}, fieldCodes(t, err))
		})
	}
}

func TestParseSearchReportsEveryError(t *testing.T) {
	_, err := core.ParseSearch(url.Values{"page": {"x"}, "page_size": {"500"}, "sort": {"rating"}})

	require.Equal(t, map[string]string{"page": "invalid_format", "page_size": "out_of_range", "sort": "invalid_format"}, fieldCodes(t, err))
}

func TestNewSearchUsesDefaultsForZeroValues(t *testing.T) {
	s, err := core.NewSearch(core.SearchInput{})

	require.NoError(t, err)
	require.Equal(t, 1, s.Page)
	require.Equal(t, 20, s.PageSize)

	_, err = core.NewSearch(core.SearchInput{Page: -1, PageSize: 101})
	require.Equal(t, map[string]string{"page": "out_of_range", "page_size": "out_of_range"}, fieldCodes(t, err))

	_, err = core.NewSearch(core.SearchInput{Page: 100001})
	require.Equal(t, map[string]string{"page": "out_of_range"}, fieldCodes(t, err))

	s, err = core.NewSearch(core.SearchInput{Page: 100000, PageSize: 100})
	require.NoError(t, err)
	require.Equal(t, 9999900, s.Offset())
}
