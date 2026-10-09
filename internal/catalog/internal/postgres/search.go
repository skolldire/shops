package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/platform/database"
)

var sortColumns = map[core.SortField]string{
	core.SortName:      "products.name",
	core.SortPrice:     "products.price",
	core.SortCreatedAt: "products.created_at",
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (r *Repository) Search(ctx context.Context, s core.Search) ([]core.Product, int, error) {
	where, args := filters(s)
	q := database.Q(ctx, r.pool)

	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM products WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("catalog: count products: %w", err)
	}

	direction := "ASC"
	if s.Descending {
		direction = "DESC"
	}
	args = append(args, s.PageSize, s.Offset())
	rows, err := q.Query(ctx,
		`SELECT `+columns+` FROM products WHERE `+where+
			` ORDER BY `+sortColumns[s.Sort]+` `+direction+`, products.id ASC`+
			` LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("catalog: search products: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.Product, error) {
		return scanProduct(row)
	})
	if err != nil {
		return nil, 0, fmt.Errorf("catalog: search products: %w", err)
	}
	return items, total, nil
}

func filters(s core.Search) (string, []any) {
	conds := []string{"status = 'ACTIVE'"}
	var args []any
	param := func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}
	if s.Q != "" {
		p := param("%" + likeEscaper.Replace(s.Q) + "%")
		conds = append(conds, `(name ILIKE `+p+` ESCAPE '\' OR sku ILIKE `+p+` ESCAPE '\' OR description ILIKE `+p+` ESCAPE '\')`)
	}
	if s.Category != "" {
		conds = append(conds, "lower(category) = lower("+param(s.Category)+")")
	}
	if s.MinPrice != nil {
		conds = append(conds, "price >= "+param(s.MinPrice.String())+"::numeric")
	}
	if s.MaxPrice != nil {
		conds = append(conds, "price <= "+param(s.MaxPrice.String())+"::numeric")
	}
	if s.InStock {
		conds = append(conds, "stock > 0")
	}
	return strings.Join(conds, " AND "), args
}
