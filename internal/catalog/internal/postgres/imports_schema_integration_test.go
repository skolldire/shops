//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImportsTableRules(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	insert := func(columns, values string) error {
		_, err := f.pool.Exec(ctx, `INSERT INTO imports (mode, file_name, file_bytes, file_hash, `+columns+`) VALUES ('add', 'f.csv', 1, 'h', `+values+`)`)
		return err
	}

	require.NoError(t, insert(`status`, `'APPLYING'`))
	require.NoError(t, insert(`status, applied_at`, `'APPLIED', now()`))
	for name, tc := range map[string][2]string{
		"an unknown status":            {`status`, `'DONE'`},
		"applied without a date":       {`status`, `'APPLIED'`},
		"a date without being applied": {`status, applied_at`, `'APPLYING', now()`},
	} {
		require.Equal(t, "23514", sqlState(insert(tc[0], tc[1])), name)
	}

	key := "6f1c1d3e-2a4b-4c5d-8e9f-0a1b2c3d4e5f"
	require.NoError(t, insert(`status, idempotency_key`, `'APPLYING', '`+key+`'`))
	require.Equal(t, "23505", sqlState(insert(`status, idempotency_key`, `'APPLYING', '`+key+`'`)), "a key belongs to one import")
}
