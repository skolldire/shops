package secret_test

import (
	"fmt"
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
