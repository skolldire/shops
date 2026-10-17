package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skolldire/shops/internal/ordering/internal/domain"
	"github.com/skolldire/shops/internal/platform/database"
)

type Inbox struct {
	pool *pgxpool.Pool
}

func NewInbox(pool *pgxpool.Pool) (*Inbox, error) {
	if pool == nil {
		return nil, errors.New("ordering: the inbox requires a pool")
	}
	return &Inbox{pool: pool}, nil
}

func (i *Inbox) Record(ctx context.Context, consumer string, eventID domain.ID) (bool, error) {
	tag, err := database.Q(ctx, i.pool).Exec(ctx,
		`INSERT INTO inbox (consumer, event_id) VALUES ($1, $2::uuid) ON CONFLICT DO NOTHING`, consumer, string(eventID))
	if err != nil {
		return false, fmt.Errorf("ordering: record the event in the inbox: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

type Sales struct {
	pool *pgxpool.Pool
}

func NewSales(pool *pgxpool.Pool) (*Sales, error) {
	if pool == nil {
		return nil, errors.New("ordering: the sales projection requires a pool")
	}
	return &Sales{pool: pool}, nil
}

func (s *Sales) AddSale(ctx context.Context, productID domain.ID, units int) error {
	_, err := database.Q(ctx, s.pool).Exec(ctx,
		`INSERT INTO product_sales (product_id, units_sold, purchases) VALUES ($1::uuid, $2, 1)
		 ON CONFLICT (product_id) DO UPDATE SET
		     units_sold = product_sales.units_sold + EXCLUDED.units_sold,
		     purchases = product_sales.purchases + 1,
		     updated_at = now()`, string(productID), units)
	if err != nil {
		return fmt.Errorf("ordering: add the sale to the projection: %w", err)
	}
	return nil
}
