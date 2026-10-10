//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/catalog/internal/core"
	"github.com/skolldire/shops/internal/catalog/internal/postgres"
	"github.com/skolldire/shops/internal/platform/database"
	"github.com/skolldire/shops/internal/platform/database/dbtest"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func dp(s string) *decimal.Decimal { v := d(s); return &v }

func sp(s string) *string { return &s }

func ip(v int64) *int64 { return &v }

type fixture struct {
	pool *pgxpool.Pool
	repo *postgres.Repository
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool, _ := dbtest.Start(t)
	repo, err := postgres.NewRepository(pool)
	require.NoError(t, err)
	return fixture{pool: pool, repo: repo}
}

func (f fixture) reset(t *testing.T) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), `TRUNCATE products`)
	require.NoError(t, err)
}

func (f fixture) create(t *testing.T, sku, name, category, price string, stock int) core.Product {
	t.Helper()
	p, err := f.repo.Create(context.Background(), core.Product{
		SKU: sku, Name: name, Description: "about " + name, Category: category,
		Price: d(price), Stock: stock, WeightKg: d("1.500"),
	})
	require.NoError(t, err)
	return p
}

func search(t *testing.T, f fixture, in core.SearchInput) ([]string, int) {
	t.Helper()
	s, err := core.NewSearch(in)
	require.NoError(t, err)
	items, total, err := f.repo.Search(context.Background(), s)
	require.NoError(t, err)
	skus := make([]string, len(items))
	for i, p := range items {
		skus[i] = p.SKU
	}
	return skus, total
}

func TestRepository(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	t.Run("create and get round trip decimals", func(t *testing.T) {
		f.reset(t)
		created, err := f.repo.Create(ctx, core.Product{
			SKU: "RS-001", Name: "Running Shoes", Category: "Sports",
			Price: d("29.99"), Stock: 10, WeightKg: d("0.001"),
		})
		require.NoError(t, err)
		require.Len(t, created.ID, 36)
		require.Equal(t, 1, created.Version)
		require.False(t, created.CreatedAt.IsZero())

		got, err := f.repo.Get(ctx, created.ID)
		require.NoError(t, err)
		require.Equal(t, "29.99", got.Price.String())
		require.Equal(t, "0.001", got.WeightKg.String())
		require.Equal(t, created, got)
	})

	t.Run("maximum values are stored without rounding", func(t *testing.T) {
		f.reset(t)
		created, err := f.repo.Create(ctx, core.Product{
			SKU: "MAX", Name: "Max", Category: "Limits",
			Price: d("9999999999.99"), Stock: 2147483647, WeightKg: d("9999999.999"),
		})
		require.NoError(t, err)
		got, err := f.repo.Get(ctx, created.ID)
		require.NoError(t, err)
		require.Equal(t, "9999999999.99", got.Price.StringFixed(2))
		require.Equal(t, 2147483647, got.Stock)
		require.Equal(t, "9999999.999", got.WeightKg.StringFixed(3))
	})

	t.Run("duplicate sku is rejected, also against a deleted product", func(t *testing.T) {
		f.reset(t)
		p := f.create(t, "DUP-1", "First", "A", "1.00", 1)
		_, err := f.repo.Create(ctx, core.Product{SKU: "DUP-1", Name: "Second", Category: "A", Price: d("1"), WeightKg: d("0")})
		require.ErrorIs(t, err, core.ErrSKUTaken)

		require.NoError(t, f.repo.Delete(ctx, p.ID, p.Version))
		_, err = f.repo.Create(ctx, core.Product{SKU: "DUP-1", Name: "Third", Category: "A", Price: d("1"), WeightKg: d("0")})
		require.ErrorIs(t, err, core.ErrSKUTaken)
	})

	t.Run("update changes only provided fields and bumps version", func(t *testing.T) {
		f.reset(t)
		p := f.create(t, "UPD-1", "Old", "Audio", "10.00", 5)
		time.Sleep(10 * time.Millisecond)

		updated, err := f.repo.Update(ctx, p.ID, p.Version, core.Changes{Name: sp("New"), Price: dp("12.50")})
		require.NoError(t, err)
		require.Equal(t, "New", updated.Name)
		require.Equal(t, "12.5", updated.Price.String())
		require.Equal(t, p.Description, updated.Description)
		require.Equal(t, p.Category, updated.Category)
		require.Equal(t, p.Stock, updated.Stock)
		require.Equal(t, p.WeightKg.String(), updated.WeightKg.String())
		require.Equal(t, 2, updated.Version)
		require.True(t, updated.UpdatedAt.After(p.UpdatedAt))
		require.Equal(t, p.CreatedAt, updated.CreatedAt)

		stocked, err := f.repo.Update(ctx, p.ID, updated.Version, core.Changes{Stock: ip(0)})
		require.NoError(t, err)
		require.Equal(t, 0, stocked.Stock)
		require.Equal(t, 3, stocked.Version)
	})

	t.Run("version conflict and not found are distinguished", func(t *testing.T) {
		f.reset(t)
		p := f.create(t, "VER-1", "Versioned", "A", "1.00", 1)
		_, err := f.repo.Update(ctx, p.ID, p.Version, core.Changes{Stock: ip(2)})
		require.NoError(t, err)

		_, err = f.repo.Update(ctx, p.ID, p.Version, core.Changes{Stock: ip(3)})
		require.ErrorIs(t, err, core.ErrVersionConflict)
		require.ErrorIs(t, f.repo.Delete(ctx, p.ID, p.Version), core.ErrVersionConflict)

		missing := "00000000-0000-4000-8000-000000000000"
		_, err = f.repo.Update(ctx, missing, 1, core.Changes{Stock: ip(1)})
		require.ErrorIs(t, err, core.ErrNotFound)
		require.ErrorIs(t, f.repo.Delete(ctx, missing, 1), core.ErrNotFound)
		_, err = f.repo.Get(ctx, missing)
		require.ErrorIs(t, err, core.ErrNotFound)
	})

	t.Run("delete is logical", func(t *testing.T) {
		f.reset(t)
		p := f.create(t, "DEL-1", "Gone", "A", "1.00", 1)

		require.NoError(t, f.repo.Delete(ctx, p.ID, p.Version))

		_, err := f.repo.Get(ctx, p.ID)
		require.ErrorIs(t, err, core.ErrNotFound)
		_, err = f.repo.Update(ctx, p.ID, p.Version+1, core.Changes{Stock: ip(1)})
		require.ErrorIs(t, err, core.ErrNotFound)
		require.ErrorIs(t, f.repo.Delete(ctx, p.ID, p.Version+1), core.ErrNotFound)
		skus, total := search(t, f, core.SearchInput{})
		require.Empty(t, skus)
		require.Zero(t, total)

		var status string
		var deletedAt *time.Time
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT status, deleted_at FROM products WHERE id = $1`, p.ID).Scan(&status, &deletedAt))
		require.Equal(t, "DELETED", status)
		require.NotNil(t, deletedAt)
	})

	t.Run("search filters", func(t *testing.T) {
		f.reset(t)
		f.create(t, "AUD-1", "Studio Headphones", "Audio", "99.90", 3)
		f.create(t, "AUD-2", "Portable Speaker", "Audio", "45.00", 0)
		f.create(t, "BOK-1", "Go Programming", "Books", "39.99", 12)
		f.create(t, "TV-1", "Smart TV", "TV", "450.00", 2)
		deleted := f.create(t, "AUD-3", "Studio Monitor", "Audio", "120.00", 1)
		require.NoError(t, f.repo.Delete(ctx, deleted.ID, deleted.Version))
		_, err := f.pool.Exec(ctx, `UPDATE products SET created_at = CASE sku
			WHEN 'AUD-1' THEN timestamptz '2026-01-01 10:00:00+00'
			WHEN 'AUD-2' THEN timestamptz '2026-01-01 10:00:01+00'
			WHEN 'BOK-1' THEN timestamptz '2026-01-01 10:00:02+00'
			WHEN 'TV-1'  THEN timestamptz '2026-01-01 10:00:03+00'
			ELSE created_at END`)
		require.NoError(t, err)

		tests := map[string]struct {
			in    core.SearchInput
			skus  []string
			total int
		}{
			"all active by name":          {core.SearchInput{}, []string{"BOK-1", "AUD-2", "TV-1", "AUD-1"}, 4},
			"q in name, case insensitive": {core.SearchInput{Q: "studio"}, []string{"AUD-1"}, 1},
			"q in sku":                    {core.SearchInput{Q: "bok"}, []string{"BOK-1"}, 1},
			"q in description":            {core.SearchInput{Q: "about smart"}, []string{"TV-1"}, 1},
			"category case insensitive":   {core.SearchInput{Category: "audio"}, []string{"AUD-2", "AUD-1"}, 2},
			"acronym category":            {core.SearchInput{Category: "tv"}, []string{"TV-1"}, 1},
			"price range":                 {core.SearchInput{MinPrice: dp("40"), MaxPrice: dp("100")}, []string{"AUD-2", "AUD-1"}, 2},
			"in stock":                    {core.SearchInput{Category: "Audio", InStock: true}, []string{"AUD-1"}, 1},
			"sort by price":               {core.SearchInput{Sort: "price"}, []string{"BOK-1", "AUD-2", "AUD-1", "TV-1"}, 4},
			"sort by price descending":    {core.SearchInput{Sort: "-price"}, []string{"TV-1", "AUD-1", "AUD-2", "BOK-1"}, 4},
			"sort by created_at":          {core.SearchInput{Sort: "created_at"}, []string{"AUD-1", "AUD-2", "BOK-1", "TV-1"}, 4},
			"first page":                  {core.SearchInput{Sort: "price", PageSize: 3}, []string{"BOK-1", "AUD-2", "AUD-1"}, 4},
			"second page":                 {core.SearchInput{Sort: "price", Page: 2, PageSize: 3}, []string{"TV-1"}, 4},
			"page past the end":           {core.SearchInput{Page: 9, PageSize: 3}, []string{}, 4},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				skus, total := search(t, f, tt.in)
				require.Equal(t, tt.skus, skus)
				require.Equal(t, tt.total, total)
			})
		}
	})

	t.Run("search escapes like wildcards", func(t *testing.T) {
		f.reset(t)
		f.create(t, "P-50", "Discount 50% off", "Deals", "1.00", 1)
		f.create(t, "P-500", "Bundle 500 units", "Deals", "1.00", 1)
		f.create(t, "P-UND", "snake_case mug", "Deals", "1.00", 1)
		f.create(t, "P-SNK", "snakeXcase mug", "Deals", "1.00", 1)
		f.create(t, "P-BSL", `back\slash poster`, "Deals", "1.00", 1)

		skus, _ := search(t, f, core.SearchInput{Q: "50%"})
		require.Equal(t, []string{"P-50"}, skus)
		skus, _ = search(t, f, core.SearchInput{Q: "snake_case"})
		require.Equal(t, []string{"P-UND"}, skus)
		skus, _ = search(t, f, core.SearchInput{Q: `back\slash`})
		require.Equal(t, []string{"P-BSL"}, skus)
	})

	t.Run("ties are broken by id for deterministic pages", func(t *testing.T) {
		f.reset(t)
		for _, sku := range []string{"T-1", "T-2", "T-3", "T-4", "T-5"} {
			f.create(t, sku, "Same", "Ties", "5.00", 1)
		}
		var seen []string
		for page := 1; page <= 3; page++ {
			skus, total := search(t, f, core.SearchInput{Sort: "price", Page: page, PageSize: 2})
			require.Equal(t, 5, total)
			seen = append(seen, skus...)
		}
		require.ElementsMatch(t, []string{"T-1", "T-2", "T-3", "T-4", "T-5"}, seen)
		require.Len(t, seen, 5)
	})

	t.Run("categories are distinct, sorted and only active", func(t *testing.T) {
		f.reset(t)
		f.create(t, "C-1", "a", "Books", "1.00", 1)
		f.create(t, "C-2", "b", "Audio", "1.00", 1)
		f.create(t, "C-3", "c", "Audio", "1.00", 1)
		gone := f.create(t, "C-4", "d", "Garden", "1.00", 1)
		require.NoError(t, f.repo.Delete(ctx, gone.ID, gone.Version))

		categories, err := f.repo.Categories(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"Audio", "Books"}, categories)
	})

	t.Run("categories that differ only by case are listed once", func(t *testing.T) {
		f.reset(t)
		f.create(t, "K-1", "Football", "Sports", "1.00", 1)
		f.create(t, "K-2", "Racket", "sports", "1.00", 1)
		f.create(t, "K-3", "Novel", "Books", "1.00", 1)
		f.create(t, "K-4", "Amplifier", "audio", "1.00", 1)

		categories, err := f.repo.Categories(ctx)
		require.NoError(t, err)
		require.Equal(t, []string{"audio", "Books", "Sports"}, categories)
		again, err := f.repo.Categories(ctx)
		require.NoError(t, err)
		require.Equal(t, categories, again)

		for _, form := range []string{"Sports", "sports", "SPORTS"} {
			skus, total := search(t, f, core.SearchInput{Category: form})
			require.Equal(t, 2, total, form)
			require.ElementsMatch(t, []string{"K-1", "K-2"}, skus, form)
		}
	})

	t.Run("participates in a shared transaction", func(t *testing.T) {
		f.reset(t)
		tm, err := database.NewTxManager(f.pool)
		require.NoError(t, err)
		rollback := errors.New("rollback")

		err = tm.WithinTx(ctx, func(ctx context.Context) error {
			_, err := f.repo.Create(ctx, core.Product{SKU: "TX-1", Name: "Tx", Category: "A", Price: d("1"), WeightKg: d("0")})
			require.NoError(t, err)
			return rollback
		})

		require.ErrorIs(t, err, rollback)
		skus, _ := search(t, f, core.SearchInput{})
		require.Empty(t, skus)
	})
}
