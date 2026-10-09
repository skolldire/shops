package config

import (
	"context"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
)

func fakeResolvers() Resolvers {
	env := map[string]string{"USER": "alice", "EMPTY": "", "lower_1": "x"}
	files := map[string]string{"/run/secrets/db": "s3cr3t\n"}
	return Resolvers{
		"env": Env(func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		}),
		"file": File(func(p string) ([]byte, error) {
			v, ok := files[p]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(v), nil
		}),
	}
}

func TestResolve(t *testing.T) {
	tests := map[string]string{
		"plain":                   "plain",
		"pg://${USER}@host":       "pg://${USER}@host",
		"${USER}":                 "alice",
		"${lower_1}":              "x",
		"${USER:-bob}":            "alice",
		"${MISSING:-bob}":         "bob",
		"${EMPTY:-bob}":           "bob",
		"${MISSING:-}":            "",
		"${env:USER}":             "alice",
		"${file:/run/secrets/db}": "s3cr3t",
	}
	for value, want := range tests {
		got, err := resolve(context.Background(), value, fakeResolvers())
		require.NoError(t, err, value)
		require.Equal(t, want, got, value)
	}
}

func TestResolveErrors(t *testing.T) {
	tests := map[string]string{
		"${MISSING}":     "${MISSING} is not set",
		"${EMPTY}":       "${EMPTY} is not set",
		"${vault:db/pw}": `${vault:db/pw}: unknown scheme "vault"`,
		"${file:/nope}":  "${file:/nope}: read file /nope: file does not exist",
		"${1ABC}":        "invalid placeholder ${1ABC}",
		"${FILE:/x}":     "invalid placeholder ${FILE:/x}",
		"${file:}":       "invalid placeholder ${file:}",
		"${A B}":         "invalid placeholder ${A B}",
	}
	for value, want := range tests {
		_, err := resolve(context.Background(), value, fakeResolvers())
		require.EqualError(t, err, want, value)
	}
}
