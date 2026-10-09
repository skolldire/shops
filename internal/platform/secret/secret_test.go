package secret_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/secret"
)

const value = "hunter2-super-secret"

func TestRevealAndIsZero(t *testing.T) {
	s := secret.New(value)

	require.Equal(t, value, s.Reveal())
	require.False(t, s.IsZero())
	require.True(t, secret.Secret{}.IsZero())
}

func TestFormattingNeverLeaks(t *testing.T) {
	s := secret.New(value)
	wrapped := struct {
		Name     string
		Password secret.Secret
	}{Name: "db", Password: s}

	outputs := map[string]string{
		"String":     s.String(),
		"GoString":   s.GoString(),
		"%v":         fmt.Sprintf("%v", s),
		"%+v":        fmt.Sprintf("%+v", s),
		"%#v":        fmt.Sprintf("%#v", s),
		"%s":         fmt.Sprintf("%s", s),
		"%q":         fmt.Sprintf("%q", s),
		"%x":         fmt.Sprintf("%x", s),
		"%d":         fmt.Sprintf("%d", s),
		"pointer":    fmt.Sprintf("%v", &s),
		"struct %v":  fmt.Sprintf("%v", wrapped),
		"struct %+v": fmt.Sprintf("%+v", wrapped),
		"struct %#v": fmt.Sprintf("%#v", wrapped),
		"Sprint":     fmt.Sprint(s),
	}
	for name, out := range outputs {
		t.Run(name, func(t *testing.T) {
			assert.NotContains(t, out, value)
			assert.Contains(t, out, "[REDACTED]")
		})
	}
}

func TestSlogJSONHandler(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	logger.Info("connecting",
		slog.Any("password", secret.New(value)),
		slog.Group("db", slog.Any("password", secret.New(value))),
	)

	require.NotContains(t, buf.String(), value)
	require.Equal(t, 2, strings.Count(buf.String(), `"password":"[REDACTED]"`))
}

func TestJSONMarshalStruct(t *testing.T) {
	cfg := struct {
		User     string        `json:"user"`
		Password secret.Secret `json:"password"`
	}{User: "shop", Password: secret.New(value)}

	b, err := json.Marshal(cfg)

	require.NoError(t, err)
	require.JSONEq(t, `{"user":"shop","password":"[REDACTED]"}`, string(b))
}
