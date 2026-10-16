package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/database"
)

type Carts struct {
	pool *pgxpool.Pool
	tx   *database.TxManager
}

func NewCarts(pool *pgxpool.Pool, tx *database.TxManager) (*Carts, error) {
	if pool == nil || tx == nil {
		return nil, errors.New("ordering: carts require a pool and a tx manager")
	}
	return &Carts{pool: pool, tx: tx}, nil
}

func (c *Carts) Lines(ctx context.Context, userID string) ([]domain.CartItem, error) {
	rows, err := database.Q(ctx, c.pool).Query(ctx,
		`SELECT product_id::text, quantity, unit_price_seen::text FROM cart_items
		 WHERE user_id = $1 ORDER BY created_at, product_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("ordering: read cart: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.CartItem, error) {
		var (
			it    domain.CartItem
			price string
		)
		if err := row.Scan(&it.ProductID, &it.Quantity, &price); err != nil {
			return domain.CartItem{}, err
		}
		var err error
		it.UnitPriceSeen, err = decimal.NewFromString(price)
		return it, err
	})
	if err != nil {
		return nil, fmt.Errorf("ordering: read cart: %w", err)
	}
	return items, nil
}

func (c *Carts) Add(ctx context.Context, userID string, item domain.CartItem, maxLines int) error {
	return c.tx.WithinTx(ctx, func(ctx context.Context) error {
		q := database.Q(ctx, c.pool)
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cart:' || $1, 0))`, userID); err != nil {
			return fmt.Errorf("ordering: lock cart: %w", err)
		}
		tag, err := q.Exec(ctx,
			`UPDATE cart_items SET quantity = LEAST(quantity + $3, $4), updated_at = now()
			 WHERE user_id = $1 AND product_id = $2::uuid`,
			userID, string(item.ProductID), item.Quantity, domain.MaxQuantity)
		if err != nil {
			return fmt.Errorf("ordering: add to cart: %w", err)
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
		var lines int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM cart_items WHERE user_id = $1`, userID).Scan(&lines); err != nil {
			return fmt.Errorf("ordering: count cart lines: %w", err)
		}
		if lines >= maxLines {
			return app.ErrCartFull
		}
		if _, err := q.Exec(ctx,
			`INSERT INTO cart_items (user_id, product_id, quantity, unit_price_seen) VALUES ($1, $2::uuid, $3, $4::numeric)`,
			userID, string(item.ProductID), item.Quantity, item.UnitPriceSeen.String()); err != nil {
			return fmt.Errorf("ordering: add to cart: %w", err)
		}
		return nil
	})
}

func (c *Carts) SetQuantity(ctx context.Context, userID string, productID domain.ID, quantity int) error {
	return c.exec(ctx, "set cart quantity",
		`UPDATE cart_items SET quantity = $3, updated_at = now() WHERE user_id = $1 AND product_id = $2::uuid`,
		userID, string(productID), quantity)
}

func (c *Carts) Remove(ctx context.Context, userID string, productID domain.ID) error {
	return c.exec(ctx, "remove from cart",
		`DELETE FROM cart_items WHERE user_id = $1 AND product_id = $2::uuid`, userID, string(productID))
}

func (c *Carts) SetPriceSeen(ctx context.Context, userID string, productID domain.ID, price decimal.Decimal) error {
	return c.exec(ctx, "accept cart price",
		`UPDATE cart_items SET unit_price_seen = $3::numeric, updated_at = now() WHERE user_id = $1 AND product_id = $2::uuid`,
		userID, string(productID), price.String())
}

func (c *Carts) RemovePurchased(ctx context.Context, userID string, items []domain.PurchaseItem) error {
	q := database.Q(ctx, c.pool)
	for _, it := range items {
		if _, err := q.Exec(ctx,
			`DELETE FROM cart_items
			 WHERE user_id = $1 AND product_id = $2::uuid AND quantity = $3 AND unit_price_seen = $4::numeric`,
			userID, string(it.ProductID), it.Quantity, it.UnitPrice.String()); err != nil {
			return fmt.Errorf("ordering: clean cart: %w", err)
		}
	}
	return nil
}

func (c *Carts) exec(ctx context.Context, op, sql string, args ...any) error {
	if _, err := database.Q(ctx, c.pool).Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("ordering: %s: %w", op, err)
	}
	return nil
}
