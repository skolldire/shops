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

const purchaseColumns = `id::text, user_id, idempotency_key::text, request_hash, status, currency, total::text,
payment_token, COALESCE(payment_ref, ''), COALESCE(failure_reason, ''), created_at`

type Purchases struct {
	pool *pgxpool.Pool
}

func NewPurchases(pool *pgxpool.Pool) (*Purchases, error) {
	if pool == nil {
		return nil, errors.New("ordering: purchases require a pool")
	}
	return &Purchases{pool: pool}, nil
}

func (r *Purchases) Insert(ctx context.Context, p domain.Purchase) (domain.Purchase, bool, error) {
	err := database.Q(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO purchases (user_id, idempotency_key, request_hash, status, currency, total, payment_token)
		 VALUES ($1, $2::uuid, $3, $4, $5, $6::numeric, $7)
		 ON CONFLICT (user_id, idempotency_key) DO NOTHING
		 RETURNING id::text, created_at`,
		p.UserID, string(p.IdempotencyKey), p.RequestHash, string(p.Status), p.Currency, p.Total.String(), p.PaymentToken).
		Scan(&p.ID, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Purchase{}, false, nil
	}
	if err != nil {
		return domain.Purchase{}, false, fmt.Errorf("ordering: insert purchase: %w", err)
	}
	return p, true, nil
}

func (r *Purchases) AddItems(ctx context.Context, purchaseID domain.ID, items []domain.PurchaseItem) error {
	ids, skus, names, prices, totals := make([]string, len(items)), make([]string, len(items)), make([]string, len(items)), make([]string, len(items)), make([]string, len(items))
	quantities := make([]int, len(items))
	for i, it := range items {
		ids[i], skus[i], names[i] = string(it.ProductID), it.SKU, it.Name
		prices[i], quantities[i], totals[i] = it.UnitPrice.String(), it.Quantity, it.LineTotal.String()
	}
	_, err := database.Q(ctx, r.pool).Exec(ctx,
		`INSERT INTO purchase_items (purchase_id, product_id, sku, name, unit_price, quantity, line_total)
		 SELECT $1::uuid, product_id, sku, name, unit_price, quantity, line_total
		 FROM unnest($2::uuid[], $3::text[], $4::text[], $5::numeric[], $6::integer[], $7::numeric[])
		      AS input(product_id, sku, name, unit_price, quantity, line_total)`,
		string(purchaseID), ids, skus, names, prices, quantities, totals)
	if err != nil {
		return fmt.Errorf("ordering: insert purchase items: %w", err)
	}
	return nil
}

func (r *Purchases) ByKey(ctx context.Context, userID string, key domain.ID) (domain.Purchase, error) {
	return r.one(ctx, `WHERE user_id = $1 AND idempotency_key = $2::uuid`, userID, string(key))
}

func (r *Purchases) Get(ctx context.Context, userID string, id domain.ID) (domain.Purchase, error) {
	return r.one(ctx, `WHERE user_id = $1 AND id = $2::uuid`, userID, string(id))
}

func (r *Purchases) List(ctx context.Context, userID string, p domain.Page) ([]domain.Purchase, int, error) {
	var total int
	if err := database.Q(ctx, r.pool).QueryRow(ctx, `SELECT count(*) FROM purchases WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("ordering: count purchases: %w", err)
	}
	items, err := r.many(ctx, `WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, userID, p.PageSize, p.Offset())
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (r *Purchases) Pending(ctx context.Context) ([]domain.Purchase, error) {
	return r.many(ctx, `WHERE status = 'PENDING' ORDER BY created_at, id`)
}

func (r *Purchases) Finish(ctx context.Context, id domain.ID, o domain.Outcome) (bool, error) {
	tag, err := database.Q(ctx, r.pool).Exec(ctx,
		`UPDATE purchases SET status = $2, payment_ref = NULLIF($3, ''), failure_reason = NULLIF($4, ''), updated_at = now()
		 WHERE id = $1::uuid AND status = 'PENDING'`,
		string(id), string(o.Status), o.PaymentRef, o.FailureReason)
	if err != nil {
		return false, fmt.Errorf("ordering: finish purchase: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *Purchases) one(ctx context.Context, where string, args ...any) (domain.Purchase, error) {
	items, err := r.many(ctx, where, args...)
	if err != nil {
		return domain.Purchase{}, err
	}
	if len(items) == 0 {
		return domain.Purchase{}, app.ErrPurchaseNotFound
	}
	return items[0], nil
}

func (r *Purchases) many(ctx context.Context, where string, args ...any) ([]domain.Purchase, error) {
	q := database.Q(ctx, r.pool)
	rows, err := q.Query(ctx, `SELECT `+purchaseColumns+` FROM purchases `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("ordering: read purchases: %w", err)
	}
	purchases, err := pgx.CollectRows(rows, scanPurchase)
	if err != nil {
		return nil, fmt.Errorf("ordering: read purchases: %w", err)
	}
	if len(purchases) == 0 {
		return purchases, nil
	}
	ids := make([]string, len(purchases))
	index := make(map[domain.ID]int, len(purchases))
	for i, p := range purchases {
		ids[i] = string(p.ID)
		index[p.ID] = i
	}
	rows, err = q.Query(ctx,
		`SELECT purchase_id::text, product_id::text, sku, name, unit_price::text, quantity, line_total::text
		 FROM purchase_items WHERE purchase_id = ANY($1::uuid[]) ORDER BY purchase_id, product_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("ordering: read purchase items: %w", err)
	}
	var (
		purchaseID   domain.ID
		item         domain.PurchaseItem
		price, total string
	)
	_, err = pgx.ForEachRow(rows, []any{&purchaseID, &item.ProductID, &item.SKU, &item.Name, &price, &item.Quantity, &total}, func() error {
		var err error
		if item.UnitPrice, err = decimal.NewFromString(price); err != nil {
			return err
		}
		if item.LineTotal, err = decimal.NewFromString(total); err != nil {
			return err
		}
		i := index[purchaseID]
		purchases[i].Items = append(purchases[i].Items, item)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ordering: read purchase items: %w", err)
	}
	return purchases, nil
}

func scanPurchase(row pgx.CollectableRow) (domain.Purchase, error) {
	var (
		p             domain.Purchase
		status, total string
	)
	if err := row.Scan(&p.ID, &p.UserID, &p.IdempotencyKey, &p.RequestHash, &status, &p.Currency, &total,
		&p.PaymentToken, &p.PaymentRef, &p.FailureReason, &p.CreatedAt); err != nil {
		return domain.Purchase{}, err
	}
	p.Status = domain.Status(status)
	var err error
	if p.Total, err = decimal.NewFromString(total); err != nil {
		return domain.Purchase{}, fmt.Errorf("ordering: parse total: %w", err)
	}
	return p, nil
}
