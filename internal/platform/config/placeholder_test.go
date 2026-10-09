package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseBody(t *testing.T) {
	tests := map[string]placeholder{
		"${USER}":          {ref: "USER"},
		"${lower_1}":       {ref: "lower_1"},
		"${PORT:-5432}":    {ref: "PORT", def: "5432", hasDefault: true},
		"${EMPTY:-}":       {ref: "EMPTY", hasDefault: true},
		"${A:-x:-y}":       {ref: "A", def: "x:-y", hasDefault: true},
		"${file:/run/s/x}": {scheme: "file", ref: "/run/s/x"},
		"${env:USER}":      {scheme: "env", ref: "USER"},
	}
	for raw, want := range tests {
		want.raw = raw
		got, err := parseBody(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, *got, raw)
	}
}

func TestParseBodyErrors(t *testing.T) {
	tests := map[string]string{
		"${1ABC}":    "invalid placeholder ${1ABC}",
		"${FILE:/x}": "invalid placeholder ${FILE:/x}",
		"${}":        "invalid placeholder ${}",
		"${A B}":     "invalid placeholder ${A B}",
		"${file:}":   "placeholder ${file:} has an empty reference",
	}
	for raw, want := range tests {
		_, err := parseBody(raw)
		require.EqualError(t, err, want, raw)
	}
}
