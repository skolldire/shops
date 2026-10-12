package httpquery_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/catalog/internal/httpquery"
	"github.com/skolldire/shops/internal/platform/validation"
)

func fieldCodes(t *testing.T, err error) map[string]string {
	t.Helper()
	var errs validation.Errors
	require.ErrorAs(t, err, &errs)
	codes := map[string]string{}
	for _, fe := range errs {
		require.NotContains(t, codes, fe.Field, "one error per field")
		require.NotEmpty(t, fe.Message)
		codes[fe.Field] = fe.Code
	}
	return codes
}

func TestParseSearchDefaults(t *testing.T) {
	s, err := httpquery.ParseSearch(url.Values{})

	require.NoError(t, err)
	require.Equal(t, domain.Search{Sort: domain.SortName, Page: 1, PageSize: 20}, s)
	require.Equal(t, 0, s.Offset())
}

func TestParseSearchValues(t *testing.T) {
	s, err := httpquery.ParseSearch(url.Values{
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
	require.Equal(t, domain.SortPrice, s.Sort)
	require.True(t, s.Descending)
	require.Equal(t, 50, s.Offset())
}

func TestParseSearchErrors(t *testing.T) {
	tests := map[string]struct {
		values url.Values
		field  string
		code   string
	}{
		"q too long":           {url.Values{"q": {strings.Repeat("q", 101)}}, "q", "too_long"},
		"min price not a num":  {url.Values{"min_price": {"cheap"}}, "min_price", "invalid_format"},
		"min price exponent":   {url.Values{"min_price": {"1e3"}}, "min_price", "invalid_format"},
		"max price huge exp":   {url.Values{"max_price": {"1e100000000"}}, "max_price", "invalid_format"},
		"max price negative":   {url.Values{"max_price": {"-1"}}, "max_price", "out_of_range"},
		"min above max":        {url.Values{"min_price": {"50"}, "max_price": {"10"}}, "min_price", "out_of_range"},
		"in_stock not bool":    {url.Values{"in_stock": {"maybe"}}, "in_stock", "invalid_format"},
		"sort not allowed":     {url.Values{"sort": {"stock"}}, "sort", "invalid_format"},
		"sort sql injection":   {url.Values{"sort": {"name; DROP TABLE products"}}, "sort", "invalid_format"},
		"page not a number":    {url.Values{"page": {"two"}}, "page", "invalid_format"},
		"page with plus sign":  {url.Values{"page": {"+2"}}, "page", "invalid_format"},
		"page with decimals":   {url.Values{"page": {"2.0"}}, "page", "invalid_format"},
		"page zero":            {url.Values{"page": {"0"}}, "page", "out_of_range"},
		"page negative":        {url.Values{"page": {"-3"}}, "page", "out_of_range"},
		"page size zero":       {url.Values{"page_size": {"0"}}, "page_size", "out_of_range"},
		"page size above 100":  {url.Values{"page_size": {"101"}}, "page_size", "out_of_range"},
		"page beyond int64":    {url.Values{"page": {"9223372036854775807"}}, "page", "out_of_range"},
		"page beyond any int":  {url.Values{"page": {"99999999999999999999"}}, "page", "out_of_range"},
		"page above maximum":   {url.Values{"page": {"100001"}}, "page", "out_of_range"},
		"min price too large":  {url.Values{"min_price": {"10000000000"}}, "min_price", "out_of_range"},
		"max price too large":  {url.Values{"max_price": {"10000000000"}}, "max_price", "out_of_range"},
		"huge min above max":   {url.Values{"min_price": {"100000000000"}, "max_price": {"10"}}, "min_price", "out_of_range"},
		"page size not number": {url.Values{"page_size": {"many"}}, "page_size", "invalid_format"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s, err := httpquery.ParseSearch(tt.values)

			require.Equal(t, map[string]string{tt.field: tt.code}, fieldCodes(t, err))
			require.Equal(t, domain.Search{}, s)
		})
	}
}

func TestParseSearchReportsFormatAndDomainErrorsTogether(t *testing.T) {
	_, err := httpquery.ParseSearch(url.Values{
		"page": {"x"}, "page_size": {"500"}, "sort": {"rating"}, "min_price": {"cheap"}, "q": {strings.Repeat("q", 101)},
	})

	require.Equal(t, map[string]string{
		"page": "invalid_format", "page_size": "out_of_range", "sort": "invalid_format",
		"min_price": "invalid_format", "q": "too_long",
	}, fieldCodes(t, err))
}

func TestParseParamsTakesTheSameTextsWithoutAQueryString(t *testing.T) {
	s, err := httpquery.ParseParams(httpquery.Params{Q: " tent ", MinPrice: "10", InStock: "true", Sort: "-price", Page: "2", PageSize: "5"})

	require.NoError(t, err)
	require.Equal(t, "tent", s.Q)
	require.Equal(t, "10", s.MinPrice.String())
	require.True(t, s.InStock)
	require.Equal(t, domain.SortPrice, s.Sort)
	require.Equal(t, 5, s.Offset())

	_, err = httpquery.ParseParams(httpquery.Params{Page: "+2", MaxPrice: "free"})
	require.Equal(t, map[string]string{"page": "invalid_format", "max_price": "invalid_format"}, fieldCodes(t, err))
}

func TestFromValuesReadsEveryParameter(t *testing.T) {
	got := httpquery.FromValues(url.Values{
		"q": {"a"}, "category": {"b"}, "min_price": {"c"}, "max_price": {"d"},
		"in_stock": {"e"}, "sort": {"f"}, "page": {"g"}, "page_size": {"h"}, "other": {"ignored"},
	})

	require.Equal(t, httpquery.Params{Q: "a", Category: "b", MinPrice: "c", MaxPrice: "d", InStock: "e", Sort: "f", Page: "g", PageSize: "h"}, got)
}

func TestParseSearchUsesTheSharedNumberMessages(t *testing.T) {
	_, err := httpquery.ParseSearch(url.Values{"page": {"+2"}, "min_price": {"1e3"}})

	var errs validation.Errors
	require.ErrorAs(t, err, &errs)
	messages := map[string]string{}
	for _, fe := range errs {
		messages[fe.Field] = fe.Message
	}
	require.Equal(t, map[string]string{
		"page":      "must be a whole number such as 25",
		"min_price": "must be a plain decimal number such as 29.99",
	}, messages)
}
