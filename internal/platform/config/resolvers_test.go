package config_test

import (
	"context"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/config"
)

func TestEnvResolver(t *testing.T) {
	env := map[string]string{"USER": "alice", "EMPTY": ""}
	r := config.Env(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})

	v, err := r.Resolve(context.Background(), "USER")
	require.NoError(t, err)
	require.Equal(t, "alice", v)

	for _, name := range []string{"MISSING", "EMPTY"} {
		_, err := r.Resolve(context.Background(), name)
		require.ErrorIs(t, err, config.ErrNotFound, name)
	}
}

func TestFileResolver(t *testing.T) {
	cases := map[string]struct{ content, want string }{
		"/lf":   {"value\n", "value"},
		"/crlf": {"value\r\n", "value"},
		"/raw":  {"value", "value"},
		"/two":  {"value\n\n", "value\n"},
	}
	r := config.File(func(p string) ([]byte, error) {
		c, ok := cases[p]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(c.content), nil
	})

	for path, c := range cases {
		got, err := r.Resolve(context.Background(), path)
		require.NoError(t, err, path)
		require.Equal(t, c.want, got, path)
	}

	_, err := r.Resolve(context.Background(), "/missing")
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.ErrorContains(t, err, "read file /missing")
}
