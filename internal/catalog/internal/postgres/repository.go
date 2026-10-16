package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/database"
)

const (
	uniqueViolation = "23505"
	skuConstraint   = "products_sku_key"
	columns         = `id::text, sku, name, description, category, price::text, stock, reserved, weight_kg::text, version, created_at, updated_at`
)

type Repository struct {
	pool *pgxpool.Pool
	tx   *database.TxManager
}

func NewRepository(pool *pgxpool.Pool) (*Repository, error) {
	if pool == nil {
		return nil, errors.New("catalog: postgres repository requires a pool")
	}
	tx, err := database.NewTxManager(pool)
	if err != nil {
		return nil, err
	}
	return &Repository{pool: pool, tx: tx}, nil
}

func (r *Repository) Create(ctx context.Context, p domain.Product) (domain.Product, error) {
	var created domain.Product
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		q := database.Q(ctx, r.pool)
		var shared bool
		if err := q.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(`+importsLockKey+`)`).Scan(&shared); err != nil {
			return fmt.Errorf("catalog: share the imports lock: %w", err)
		}
		if !shared {
			return app.ErrImportInProgress
		}
		if err := setStockReason(ctx, q, reasonInitialStock, ""); err != nil {
			return err
		}
		row := q.QueryRow(ctx,
			`INSERT INTO products (sku, name, description, category, price, stock, weight_kg)
			 VALUES ($1, $2, $3, $4, $5::numeric, $6, $7::numeric)
			 RETURNING `+columns,
			p.SKU, p.Name, p.Description, p.Category, p.Price.String(), p.Stock, p.WeightKg.String())
		var err error
		created, err = scanProduct(row)
		if isSKUTaken(err) {
			return app.ErrSKUTaken
		}
		if err != nil {
			return fmt.Errorf("catalog: insert product: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Product{}, err
	}
	return created, nil
}

func (r *Repository) Get(ctx context.Context, id string) (domain.Product, error) {
	row := database.Q(ctx, r.pool).QueryRow(ctx,
		`SELECT `+columns+` FROM products WHERE id = $1::uuid AND status = 'ACTIVE'`, id)
	p, err := scanProduct(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Product{}, app.ErrNotFound
	}
	if err != nil {
		return domain.Product{}, fmt.Errorf("catalog: get product: %w", err)
	}
	return p, nil
}

func (r *Repository) Update(ctx context.Context, id string, version int, c domain.Changes) (domain.Product, error) {
	set := []string{"version = version + 1", "updated_at = now()"}
	args := []any{id, version}
	add := func(column, cast string, value any) {
		args = append(args, value)
		set = append(set, column+" = $"+strconv.Itoa(len(args))+cast)
	}
	if c.Name != nil {
		add("name", "", *c.Name)
	}
	if c.Description != nil {
		add("description", "", *c.Description)
	}
	if c.Category != nil {
		add("category", "", *c.Category)
	}
	if c.Price != nil {
		add("price", "::numeric", c.Price.String())
	}
	where := ""
	if c.Stock != nil {
		add("stock", "", *c.Stock)
		where = " AND $" + strconv.Itoa(len(args)) + " >= reserved"
	}
	if c.WeightKg != nil {
		add("weight_kg", "::numeric", c.WeightKg.String())
	}

	var updated domain.Product
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		q := database.Q(ctx, r.pool)
		if err := setStockReason(ctx, q, reasonManualAdjustment, ""); err != nil {
			return err
		}
		row := q.QueryRow(ctx,
			`UPDATE products SET `+strings.Join(set, ", ")+`
			 WHERE id = $1::uuid AND version = $2 AND status = 'ACTIVE'`+where+`
			 RETURNING `+columns, args...)
		var err error
		updated, err = scanProduct(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return r.whyNotUpdated(ctx, id, version, c.Stock != nil)
		}
		if err != nil {
			return fmt.Errorf("catalog: update product: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Product{}, err
	}
	return updated, nil
}

func (r *Repository) Delete(ctx context.Context, id string, version int) error {
	tag, err := database.Q(ctx, r.pool).Exec(ctx,
		`UPDATE products
		 SET status = 'DELETED', deleted_at = now(), updated_at = now(), version = version + 1
		 WHERE id = $1::uuid AND version = $2 AND status = 'ACTIVE'`, id, version)
	if err != nil {
		return fmt.Errorf("catalog: delete product: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return r.missOrConflict(ctx, id)
	}
	return nil
}

func (r *Repository) Categories(ctx context.Context) ([]string, error) {
	rows, err := database.Q(ctx, r.pool).Query(ctx,
		`SELECT min(category COLLATE "C") FROM products WHERE status = 'ACTIVE'
		 GROUP BY lower(category) ORDER BY lower(category)`)
	if err != nil {
		return nil, fmt.Errorf("catalog: list categories: %w", err)
	}
	categories, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("catalog: list categories: %w", err)
	}
	return categories, nil
}

func (r *Repository) whyNotUpdated(ctx context.Context, id string, version int, stockChanged bool) error {
	var current, reserved int
	err := database.Q(ctx, r.pool).QueryRow(ctx,
		`SELECT version, reserved FROM products WHERE id = $1::uuid AND status = 'ACTIVE'`, id).Scan(&current, &reserved)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return app.ErrNotFound
	case err != nil:
		return fmt.Errorf("catalog: check product: %w", err)
	case current != version, !stockChanged:
		return app.ErrVersionConflict
	}
	return &app.BelowReservedError{Reserved: reserved}
}

func (r *Repository) missOrConflict(ctx context.Context, id string) error {
	var exists bool
	err := database.Q(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM products WHERE id = $1::uuid AND status = 'ACTIVE')`, id).Scan(&exists)
	switch {
	case err != nil:
		return fmt.Errorf("catalog: check product: %w", err)
	case exists:
		return app.ErrVersionConflict
	default:
		return app.ErrNotFound
	}
}

func scanProduct(row pgx.Row) (domain.Product, error) {
	var (
		p             domain.Product
		price, weight string
	)
	if err := row.Scan(&p.ID, &p.SKU, &p.Name, &p.Description, &p.Category,
		&price, &p.Stock, &p.Reserved, &weight, &p.Version, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return domain.Product{}, err
	}
	var err error
	if p.Price, err = decimal.NewFromString(price); err != nil {
		return domain.Product{}, fmt.Errorf("catalog: parse price: %w", err)
	}
	if p.WeightKg, err = decimal.NewFromString(weight); err != nil {
		return domain.Product{}, fmt.Errorf("catalog: parse weight: %w", err)
	}
	return p, nil
}

func isSKUTaken(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == skuConstraint
}
