package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

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
RETURNING sku, (xmax = 0), COALESCE(old.stock, 0), new.stock, COALESCE(old.price::text, '0'), new.price::text`

const upsertAdd = upsertPrefix + `
    stock = products.stock + EXCLUDED.stock
WHERE products.status = 'ACTIVE'
  AND (EXCLUDED.stock <> 0
       OR (products.name, products.description, products.category, products.price, products.weight_kg)
          IS DISTINCT FROM
          (EXCLUDED.name, EXCLUDED.description, EXCLUDED.category, EXCLUDED.price, EXCLUDED.weight_kg))
RETURNING sku, (xmax = 0), COALESCE(old.stock, 0), new.stock, COALESCE(old.price::text, '0'), new.price::text`

const lockExistingByID = `SELECT id FROM products WHERE sku = ANY($1) ORDER BY id FOR UPDATE`

const deletedSKUs = `SELECT sku FROM products WHERE sku = ANY($1) AND status = 'DELETED'`

const belowReservedSKUs = `SELECT p.sku FROM products p
JOIN unnest($1::text[], $2::integer[]) AS input(sku, stock) ON input.sku = p.sku
WHERE p.status = 'ACTIVE' AND p.reserved > input.stock`

func (r *Repository) Upsert(ctx context.Context, mode domain.ImportMode, products []domain.Product, reference string) (app.UpsertResult, error) {
	statement, reason := upsertReplace, reasonImportReplace
	if mode == domain.ImportAdd {
		statement, reason = upsertAdd, reasonImportAdd
	}
	outcomes := make(map[string]app.UpsertOutcome, len(products))
	changes := make(map[string]domain.ImportChange, len(products))
	if len(products) == 0 {
		return app.UpsertResult{Outcomes: outcomes, Changes: changes}, nil
	}
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		clear(outcomes)
		clear(changes)
		q := database.Q(ctx, r.pool)
		if err := setStockReason(ctx, q, reason, reference); err != nil {
			return err
		}

		skus := make([]string, len(products))
		for i, p := range products {
			skus[i] = p.SKU
		}
		if err := lockExisting(ctx, q, skus); err != nil {
			return err
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
			sku                     string
			inserted                bool
			change                  domain.ImportChange
			priceBefore, priceAfter string
		)
		_, err = pgx.ForEachRow(rows, []any{&sku, &inserted, &change.StockBefore, &change.StockAfter, &priceBefore, &priceAfter}, func() error {
			if inserted {
				outcomes[sku] = app.OutcomeCreated
				return nil
			}
			outcomes[sku] = app.OutcomeUpdated
			var err error
			if change.PriceBefore, err = decimal.NewFromString(priceBefore); err != nil {
				return err
			}
			if change.PriceAfter, err = decimal.NewFromString(priceAfter); err != nil {
				return err
			}
			change.SKU = sku
			changes[sku] = change
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
		if mode == domain.ImportReplace {
			if err := markBelowReserved(ctx, q, untouched, stocks, outcomes); err != nil {
				return err
			}
		}
		return unchangedValues(ctx, q, untouched, outcomes, changes)
	})
	if err != nil {
		return app.UpsertResult{}, err
	}
	return app.UpsertResult{Outcomes: outcomes, Changes: changes}, nil
}

func (r *Repository) LockExisting(ctx context.Context, skus []string) error {
	return lockExisting(ctx, database.Q(ctx, r.pool), skus)
}

func lockExisting(ctx context.Context, q database.Querier, skus []string) error {
	if len(skus) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, lockExistingByID, skus); err != nil {
		return fmt.Errorf("catalog: lock the existing products: %w", err)
	}
	return nil
}

func unchangedValues(ctx context.Context, q database.Querier, skus []string, outcomes map[string]app.UpsertOutcome, changes map[string]domain.ImportChange) error {
	if len(skus) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT sku, stock, price::text FROM products WHERE sku = ANY($1)`, skus)
	if err != nil {
		return fmt.Errorf("catalog: read unchanged products: %w", err)
	}
	var (
		sku, price string
		stock      int
	)
	_, err = pgx.ForEachRow(rows, []any{&sku, &stock, &price}, func() error {
		if outcomes[sku] != app.OutcomeUnchanged {
			return nil
		}
		d, err := decimal.NewFromString(price)
		if err != nil {
			return err
		}
		changes[sku] = domain.ImportChange{SKU: sku, StockBefore: stock, StockAfter: stock, PriceBefore: d, PriceAfter: d}
		return nil
	})
	if err != nil {
		return fmt.Errorf("catalog: read unchanged products: %w", err)
	}
	return nil
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

func (r *Repository) LockSeed(ctx context.Context) (context.Context, func(), error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: acquire a connection for the seed lock: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('catalog.seed', 0))`); err != nil {
		conn.Release()
		return nil, nil, fmt.Errorf("catalog: lock the demo seed: %w", err)
	}
	return database.WithConn(ctx, conn), func() {
		releaseAdvisory(ctx, conn, `SELECT pg_advisory_unlock(hashtextextended('catalog.seed', 0))`)
	}, nil
}

const advisoryReleaseTimeout = 5 * time.Second

func releaseAdvisory(ctx context.Context, conn *pgxpool.Conn, unlock string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), advisoryReleaseTimeout)
	defer cancel()
	var released bool
	if err := conn.QueryRow(ctx, unlock).Scan(&released); err != nil || !released {
		_ = conn.Conn().Close(ctx)
	}
	conn.Release()
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
