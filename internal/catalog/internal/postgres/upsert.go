package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/platform/database"
)

const upsertProducts = `
INSERT INTO products (sku, name, description, category, price, stock, weight_kg)
SELECT sku, name, description, category, price::numeric, stock, weight_kg::numeric
FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::integer[], $7::text[])
     AS input(sku, name, description, category, price, stock, weight_kg)
ORDER BY sku
ON CONFLICT (sku) DO UPDATE SET
    name = EXCLUDED.name,
    description = EXCLUDED.description,
    category = EXCLUDED.category,
    price = EXCLUDED.price,
    stock = EXCLUDED.stock,
    weight_kg = EXCLUDED.weight_kg,
    version = products.version + 1,
    updated_at = now()
WHERE products.status = 'ACTIVE'
  AND (products.name, products.description, products.category, products.price, products.stock, products.weight_kg)
      IS DISTINCT FROM
      (EXCLUDED.name, EXCLUDED.description, EXCLUDED.category, EXCLUDED.price, EXCLUDED.stock, EXCLUDED.weight_kg)
RETURNING sku, (xmax = 0)`

func (r *Repository) Upsert(ctx context.Context, products []core.Product) (map[string]core.UpsertOutcome, error) {
	outcomes := make(map[string]core.UpsertOutcome, len(products))
	if len(products) == 0 {
		return outcomes, nil
	}
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		clear(outcomes)
		q := database.Q(ctx, r.pool)

		skus := make([]string, len(products))
		for i, p := range products {
			skus[i] = p.SKU
		}
		deleted, err := collect(q.Query(ctx, `SELECT sku FROM products WHERE sku = ANY($1) AND status = 'DELETED'`, skus))
		if err != nil {
			return fmt.Errorf("catalog: find deleted skus: %w", err)
		}
		for _, sku := range deleted {
			outcomes[sku] = core.OutcomeDeleted
		}

		var in upsertInput
		for _, p := range products {
			if _, gone := outcomes[p.SKU]; gone {
				continue
			}
			outcomes[p.SKU] = core.OutcomeUnchanged
			in.add(p)
		}
		if len(in.skus) == 0 {
			return nil
		}

		rows, err := q.Query(ctx, upsertProducts,
			in.skus, in.names, in.descriptions, in.categories, in.prices, in.stocks, in.weights)
		if err != nil {
			return fmt.Errorf("catalog: upsert products: %w", err)
		}
		var (
			sku      string
			inserted bool
		)
		_, err = pgx.ForEachRow(rows, []any{&sku, &inserted}, func() error {
			outcomes[sku] = core.OutcomeUpdated
			if inserted {
				outcomes[sku] = core.OutcomeCreated
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("catalog: upsert products: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcomes, nil
}

func (r *Repository) HasProducts(ctx context.Context) (bool, error) {
	var exists bool
	if err := database.Q(ctx, r.pool).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM products)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("catalog: check for products: %w", err)
	}
	return exists, nil
}

type upsertInput struct {
	skus, names, descriptions, categories, prices, weights []string
	stocks                                                 []int
}

func (in *upsertInput) add(p core.Product) {
	in.skus = append(in.skus, p.SKU)
	in.names = append(in.names, p.Name)
	in.descriptions = append(in.descriptions, p.Description)
	in.categories = append(in.categories, p.Category)
	in.prices = append(in.prices, p.Price.String())
	in.stocks = append(in.stocks, p.Stock)
	in.weights = append(in.weights, p.WeightKg.String())
}

func collect(rows pgx.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
