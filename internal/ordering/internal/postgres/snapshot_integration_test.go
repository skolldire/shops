//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/platform/database/dbtest"
)

func TestPurchaseSnapshotsAreImmutable(t *testing.T) {
	pool, _ := dbtest.Start(t)
	ctx := context.Background()
	var id string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO purchases (user_id, idempotency_key, request_hash, status, total, payment_token)
		 VALUES ('alice', gen_random_uuid(), 'hash', 'PENDING', 59.98, 'pm_approved') RETURNING id::text`).Scan(&id))
	_, err := pool.Exec(ctx,
		`INSERT INTO purchase_items (purchase_id, product_id, sku, name, unit_price, quantity, line_total)
		 VALUES ($1::uuid, gen_random_uuid(), 'A-1', 'Alpha', 29.99, 2, 59.98)`, id)
	require.NoError(t, err)

	t.Run("the amount, identity and payment token of a purchase cannot change", func(t *testing.T) {
		for _, set := range []string{
			`id = gen_random_uuid()`,
			`user_id = 'bob'`,
			`idempotency_key = gen_random_uuid()`,
			`request_hash = 'other'`,
			`currency = 'EUR'`,
			`total = 39.99`,
			`payment_token = 'pm_declined'`,
			`created_at = now() - interval '1 day'`,
		} {
			_, err := pool.Exec(ctx, `UPDATE purchases SET `+set+` WHERE id = $1::uuid`, id)
			require.Equal(t, "42501", sqlState(err), set)
		}
	})

	t.Run("the items of a purchase cannot be changed, deleted or truncated", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE purchase_items SET unit_price = 39.99, line_total = 79.98`,
			`UPDATE purchase_items SET quantity = 1`,
			`DELETE FROM purchase_items`,
			`TRUNCATE purchase_items`,
		} {
			_, err := pool.Exec(ctx, stmt)
			require.Equal(t, "42501", sqlState(err), stmt)
		}
	})

	t.Run("the outcome of a purchase can still be recorded", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`UPDATE purchases SET status = 'CONFIRMED', payment_ref = 'ref', failure_reason = NULL, updated_at = now()
			 WHERE id = $1::uuid`, id)
		require.NoError(t, err)
	})

	var status, total, price, line string
	var quantity int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT p.status, p.total::text, i.unit_price::text, i.quantity, i.line_total::text
		 FROM purchases p JOIN purchase_items i ON i.purchase_id = p.id WHERE p.id = $1::uuid`, id).
		Scan(&status, &total, &price, &quantity, &line))
	require.Equal(t, []any{"CONFIRMED", "59.98", "29.99", 2, "59.98"}, []any{status, total, price, quantity, line})
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
