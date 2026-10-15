package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/database"
)

const upsertPrefix = `
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
    weight_kg = EXCLUDED.weight_kg,
    version = products.version + 1,
    updated_at = now(),`

const upsertReplace = upsertPrefix + `
    stock = EXCLUDED.stock
WHERE products.status = 'ACTIVE'
  AND EXCLUDED.stock >= products.reserved
  AND (products.name, products.description, products.category, products.price, products.stock, products.weight_kg)
      IS DISTINCT FROM
      (EXCLUDED.name, EXCLUDED.description, EXCLUDED.category, EXCLUDED.price, EXCLUDED.stock, EXCLUDED.weight_kg)
RETURNING sku, (xmax = 0)`

const upsertAdd = upsertPrefix + `
    stock = products.stock + EXCLUDED.stock
WHERE products.status = 'ACTIVE'
  AND (EXCLUDED.stock <> 0
       OR (products.name, products.description, products.category, products.price, products.weight_kg)
          IS DISTINCT FROM
          (EXCLUDED.name, EXCLUDED.description, EXCLUDED.category, EXCLUDED.price, EXCLUDED.weight_kg))
RETURNING sku, (xmax = 0)`

const deletedSKUs = `SELECT sku FROM products WHERE sku = ANY($1) AND status = 'DELETED'`

const belowReservedSKUs = `SELECT p.sku FROM products p
JOIN unnest($1::text[], $2::integer[]) AS input(sku, stock) ON input.sku = p.sku
WHERE p.status = 'ACTIVE' AND p.reserved > input.stock`

func (r *Repository) Upsert(ctx context.Context, mode domain.ImportMode, products []domain.Product, reference string) (map[string]app.UpsertOutcome, error) {
	statement, reason := upsertReplace, reasonImportReplace
	if mode == domain.ImportAdd {
		statement, reason = upsertAdd, reasonImportAdd
	}
	outcomes := make(map[string]app.UpsertOutcome, len(products))
	if len(products) == 0 {
		return outcomes, nil
	}
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		clear(outcomes)
		q := database.Q(ctx, r.pool)
		if err := setStockReason(ctx, q, reason, reference); err != nil {
			return err
		}

		skus := make([]string, len(products))
		for i, p := range products {
			skus[i] = p.SKU
		}
		if err := markDeleted(ctx, q, skus, outcomes); err != nil {
			return err
		}

		var in upsertInput
		for _, p := range products {
			if _, gone := outcomes[p.SKU]; gone {
				continue
			}
			outcomes[p.SKU] = app.OutcomeUnchanged
			in.add(p)
		}
		if len(in.skus) == 0 {
			return nil
		}

		rows, err := q.Query(ctx, statement,
			in.skus, in.names, in.descriptions, in.categories, in.prices, in.stocks, in.weights)
		if err != nil {
			return fmt.Errorf("catalog: upsert products: %w", err)
		}
		var (
			sku      string
			inserted bool
		)
		_, err = pgx.ForEachRow(rows, []any{&sku, &inserted}, func() error {
			outcomes[sku] = app.OutcomeUpdated
			if inserted {
				outcomes[sku] = app.OutcomeCreated
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("catalog: upsert products: %w", err)
		}

		var (
			untouched []string
			stocks    []int
		)
		for i, sku := range in.skus {
			if outcomes[sku] == app.OutcomeUnchanged {
				untouched = append(untouched, sku)
				stocks = append(stocks, in.stocks[i])
			}
		}
		if err := markDeleted(ctx, q, untouched, outcomes); err != nil {
			return err
		}
		if mode == domain.ImportAdd {
			return nil
		}
		return markBelowReserved(ctx, q, untouched, stocks, outcomes)
	})
	if err != nil {
		return nil, err
	}
	return outcomes, nil
}

func markDeleted(ctx context.Context, q database.Querier, skus []string, outcomes map[string]app.UpsertOutcome) error {
	if len(skus) == 0 {
		return nil
	}
	deleted, err := collect(q.Query(ctx, deletedSKUs, skus))
	if err != nil {
		return fmt.Errorf("catalog: find deleted skus: %w", err)
	}
	for _, sku := range deleted {
		outcomes[sku] = app.OutcomeDeleted
	}
	return nil
}

func markBelowReserved(ctx context.Context, q database.Querier, skus []string, stocks []int, outcomes map[string]app.UpsertOutcome) error {
	if len(skus) == 0 {
		return nil
	}
	below, err := collect(q.Query(ctx, belowReservedSKUs, skus, stocks))
	if err != nil {
		return fmt.Errorf("catalog: find skus below their reserved units: %w", err)
	}
	for _, sku := range below {
		outcomes[sku] = app.OutcomeBelowReserved
	}
	return nil
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

func (in *upsertInput) add(p domain.Product) {
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
