package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/database"
)

const takeStock = `UPDATE products
SET stock = stock - $2, version = version + 1, updated_at = now()
WHERE id = $1::uuid AND status = 'ACTIVE' AND stock >= $2
RETURNING sku, name, price::text`

const returnStock = `UPDATE products
SET stock = stock + $2, version = version + 1, updated_at = now()
WHERE id = $1::uuid`

func (r *Repository) TakeStock(ctx context.Context, items []domain.StockRequest, reference string) ([]domain.StockLine, error) {
	lines := make([]domain.StockLine, 0, len(items))
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		lines = lines[:0]
		q := database.Q(ctx, r.pool)
		if err := setStockReason(ctx, q, reasonSale, reference); err != nil {
			return err
		}
		for _, it := range items {
			line := domain.StockLine{ProductID: it.ProductID, Quantity: it.Quantity}
			var price string
			err := q.QueryRow(ctx, takeStock, it.ProductID, it.Quantity).Scan(&line.SKU, &line.Name, &price)
			if errors.Is(err, pgx.ErrNoRows) {
				return r.stockShortage(ctx, q, it.ProductID)
			}
			if err != nil {
				return fmt.Errorf("catalog: take stock: %w", err)
			}
			if line.Price, err = decimal.NewFromString(price); err != nil {
				return fmt.Errorf("catalog: parse price: %w", err)
			}
			lines = append(lines, line)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return lines, nil
}

func (r *Repository) stockShortage(ctx context.Context, q database.Querier, id string) error {
	var (
		status string
		stock  int
	)
	err := q.QueryRow(ctx, `SELECT status, stock FROM products WHERE id = $1::uuid`, id).Scan(&status, &stock)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return app.ErrProductUnavailable
	case err != nil:
		return fmt.Errorf("catalog: check stock: %w", err)
	case status != "ACTIVE":
		return app.ErrProductUnavailable
	}
	return &app.InsufficientStockError{ProductID: id, Available: stock}
}

func (r *Repository) ReturnStock(ctx context.Context, items []domain.StockRequest, reference string) error {
	return r.tx.WithinTx(ctx, func(ctx context.Context) error {
		q := database.Q(ctx, r.pool)
		if err := setStockReason(ctx, q, reasonSaleReversal, reference); err != nil {
			return err
		}
		for _, it := range items {
			tag, err := q.Exec(ctx, returnStock, it.ProductID, it.Quantity)
			if err != nil {
				return fmt.Errorf("catalog: return stock: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("catalog: return stock: product %s does not exist", it.ProductID)
			}
		}
		return nil
	})
}

func (r *Repository) Movements(ctx context.Context, productID string, p domain.Paging) ([]domain.Movement, int, error) {
	q := database.Q(ctx, r.pool)
	var (
		exists bool
		total  int
	)
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM products WHERE id = $1::uuid),
		        (SELECT count(*) FROM inventory_movements WHERE product_id = $1::uuid)`, productID).Scan(&exists, &total)
	if err != nil {
		return nil, 0, fmt.Errorf("catalog: count movements: %w", err)
	}
	if !exists {
		return nil, 0, app.ErrNotFound
	}
	rows, err := q.Query(ctx,
		`SELECT id, product_id::text, delta, stock_after, reason, COALESCE(reference, ''), db_user, tx_id::text, created_at
		 FROM inventory_movements WHERE product_id = $1::uuid
		 ORDER BY id DESC LIMIT $2 OFFSET $3`, productID, p.PageSize, p.Offset())
	if err != nil {
		return nil, 0, fmt.Errorf("catalog: list movements: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Movement, error) {
		var m domain.Movement
		err := row.Scan(&m.ID, &m.ProductID, &m.Delta, &m.StockAfter, &m.Reason, &m.Reference, &m.DBUser, &m.TxID, &m.CreatedAt)
		return m, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("catalog: list movements: %w", err)
	}
	return items, total, nil
}
