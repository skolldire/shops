package domain_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/domain"
)

func TestParseImportMode(t *testing.T) {
	for raw, want := range map[string]domain.ImportMode{"add": domain.ImportAdd, "replace": domain.ImportReplace, "": domain.ImportReplace} {
		got, err := domain.ParseImportMode(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	for _, raw := range []string{"ADD", "sum", " add"} {
		_, err := domain.ParseImportMode(raw)
		require.ErrorIs(t, err, domain.ErrInvalidImportMode, raw)
	}
}
