package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

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

func (r *Purchases) Claim(ctx context.Context, owner string, minAge, lease time.Duration, batch int) ([]domain.Purchase, error) {
	rows, err := database.Q(ctx, r.pool).Query(ctx,
		`UPDATE purchases
		 SET lease_until = now() + $2 * interval '1 microsecond', lease_owner = $1::uuid
		 WHERE id IN (
		     SELECT id FROM purchases
		     WHERE status = 'PENDING'
		       AND created_at < now() - $3 * interval '1 microsecond'
		       AND next_attempt_at <= now()
		       AND (lease_until IS NULL OR lease_until < now())
		     ORDER BY next_attempt_at, id
		     LIMIT $4
		     FOR UPDATE SKIP LOCKED)
		 RETURNING id::text`,
		owner, lease.Microseconds(), minAge.Microseconds(), batch)
	if err != nil {
		return nil, fmt.Errorf("ordering: claim pending purchases: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("ordering: claim pending purchases: %w", err)
	}
	if len(ids) == 0 {
		return []domain.Purchase{}, nil
	}
	return r.many(ctx, `WHERE id = ANY($1::uuid[]) ORDER BY next_attempt_at, id`, ids)
}

func (r *Purchases) Renew(ctx context.Context, id domain.ID, owner string, lease, maxPendingAge time.Duration) (app.RenewResult, bool, error) {
	var renewed app.RenewResult
	err := database.Q(ctx, r.pool).QueryRow(ctx,
		`UPDATE purchases SET lease_until = now() + $3 * interval '1 microsecond'
		 WHERE id = $1::uuid AND lease_owner = $2::uuid AND status = 'PENDING'
		 RETURNING recovery_attempts, created_at < now() - $4 * interval '1 microsecond'`,
		string(id), owner, lease.Microseconds(), maxPendingAge.Microseconds()).Scan(&renewed.Attempts, &renewed.Overdue)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return app.RenewResult{}, false, nil
	case err != nil:
		return app.RenewResult{}, false, fmt.Errorf("ordering: renew the recovery lease: %w", err)
	}
	return renewed, true, nil
}

func (r *Purchases) Reschedule(ctx context.Context, id domain.ID, owner string, backoff time.Duration, cause string) (int, bool, error) {
	var attempts int
	err := database.Q(ctx, r.pool).QueryRow(ctx,
		`UPDATE purchases
		 SET recovery_attempts = recovery_attempts + 1,
		     next_attempt_at = now() + $3 * interval '1 microsecond',
		     lease_until = NULL, lease_owner = NULL, last_recovery_error = $4
		 WHERE id = $1::uuid AND lease_owner = $2::uuid AND status = 'PENDING'
		 RETURNING recovery_attempts`,
		string(id), owner, backoff.Microseconds(), cause).Scan(&attempts)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("ordering: reschedule the recovery: %w", err)
	}
	return attempts, true, nil
}

func (r *Purchases) PendingStats(ctx context.Context, stuckAfter int) (app.PendingStats, error) {
	var stats app.PendingStats
	err := database.Q(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*),
		        COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)::float8,
		        count(*) FILTER (WHERE recovery_attempts >= $1),
		        COALESCE((SELECT sum(i.quantity) FROM purchase_items i JOIN purchases s ON s.id = i.purchase_id
		                  WHERE s.status = 'PENDING' AND s.recovery_attempts >= $1), 0)
		 FROM purchases WHERE status = 'PENDING'`, stuckAfter).
		Scan(&stats.Pending, &stats.OldestSeconds, &stats.Stuck, &stats.StuckReservedUnits)
	if err != nil {
		return app.PendingStats{}, fmt.Errorf("ordering: read the pending purchases: %w", err)
	}
	return stats, nil
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
