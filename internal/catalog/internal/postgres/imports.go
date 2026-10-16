package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/database"
)

const importColumns = `id::text, COALESCE(idempotency_key::text, ''), mode, status, file_name, file_bytes, file_hash,
records, skipped, created, updated, unchanged, rejected, errors, changes, created_at, applied_at`

type ImportLog struct {
	pool *pgxpool.Pool
}

func NewImportLog(pool *pgxpool.Pool) (*ImportLog, error) {
	if pool == nil {
		return nil, errors.New("catalog: the import log requires a pool")
	}
	return &ImportLog{pool: pool}, nil
}

func (l *ImportLog) Create(ctx context.Context, rec app.ImportRecord) (app.ImportRecord, bool, error) {
	var id string
	err := database.Q(ctx, l.pool).QueryRow(ctx,
		`INSERT INTO imports (idempotency_key, mode, status, file_name, file_bytes, file_hash)
		 VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5, $6)
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING id::text`,
		rec.Key, string(rec.Mode), string(rec.Status), rec.File.Name, rec.File.Bytes, rec.File.SHA256).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := l.one(ctx, `idempotency_key = $1::uuid`, rec.Key)
		return existing, false, err
	}
	if err != nil {
		return app.ImportRecord{}, false, fmt.Errorf("catalog: insert import: %w", err)
	}
	created, err := l.Get(ctx, id)
	return created, true, err
}

func (l *ImportLog) ByKey(ctx context.Context, key string) (app.ImportRecord, bool, error) {
	rec, err := l.one(ctx, `idempotency_key = $1::uuid`, key)
	switch {
	case errors.Is(err, app.ErrImportNotFound):
		return app.ImportRecord{}, false, nil
	case err != nil:
		return app.ImportRecord{}, false, err
	}
	return rec, true, nil
}

func (l *ImportLog) LockImports(ctx context.Context) (context.Context, func(), bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, false, fmt.Errorf("catalog: acquire a connection for the imports lock: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('catalog.imports', 0))`).Scan(&locked); err != nil {
		conn.Release()
		return nil, nil, false, fmt.Errorf("catalog: lock the imports: %w", err)
	}
	if !locked {
		conn.Release()
		return ctx, func() {}, false, nil
	}
	return database.WithConn(ctx, conn), func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended('catalog.imports', 0))`)
		conn.Release()
	}, true, nil
}

func (l *ImportLog) Get(ctx context.Context, id string) (app.ImportRecord, error) {
	return l.one(ctx, `id = $1::uuid`, id)
}

func (l *ImportLog) one(ctx context.Context, where, arg string) (app.ImportRecord, error) {
	rec, err := scanImport(database.Q(ctx, l.pool).QueryRow(ctx, `SELECT `+importColumns+` FROM imports WHERE `+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.ImportRecord{}, app.ErrImportNotFound
	}
	if err != nil {
		return app.ImportRecord{}, fmt.Errorf("catalog: read import: %w", err)
	}
	return rec, nil
}

func scanImport(row pgx.Row) (app.ImportRecord, error) {
	var (
		rec           app.ImportRecord
		mode, status  string
		errs, changes []byte
		appliedAt     *time.Time
		r             = &rec.Report
	)
	if err := row.Scan(&rec.ID, &rec.Key, &mode, &status, &rec.File.Name, &rec.File.Bytes, &rec.File.SHA256,
		&r.Records, &r.Skipped, &r.Created, &r.Updated, &r.Unchanged, &r.Rejected,
		&errs, &changes, &rec.Stamp.CreatedAt, &appliedAt); err != nil {
		return app.ImportRecord{}, err
	}
	rec.Mode, rec.Status = domain.ImportMode(mode), domain.ImportStatus(status)
	if appliedAt != nil {
		rec.Stamp.AppliedAt = *appliedAt
	}
	if err := json.Unmarshal(errs, &r.Errors); err != nil {
		return app.ImportRecord{}, fmt.Errorf("decode the import errors: %w", err)
	}
	var err error
	if r.Changes, err = decodeChanges(changes); err != nil {
		return app.ImportRecord{}, fmt.Errorf("decode the import changes: %w", err)
	}
	if r.Errors == nil {
		r.Errors = []app.RowError{}
	}
	r.ID, r.Mode, r.File = rec.ID, rec.Mode, rec.File
	return rec, nil
}

func (l *ImportLog) LastAppliedAdd(ctx context.Context, hash, exceptID string) (domain.ImportStamp, bool, error) {
	var stamp domain.ImportStamp
	q := database.Q(ctx, l.pool)
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('catalog.import.file:' || $1, 0))`, hash); err != nil {
		return domain.ImportStamp{}, false, fmt.Errorf("catalog: lock the file of an add import: %w", err)
	}
	err := q.QueryRow(ctx,
		`SELECT created_at, applied_at FROM imports
		 WHERE mode = 'add' AND status = 'APPLIED' AND file_hash = $1 AND id IS DISTINCT FROM NULLIF($2, '')::uuid
		 ORDER BY applied_at DESC LIMIT 1`, hash, exceptID).Scan(&stamp.CreatedAt, &stamp.AppliedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.ImportStamp{}, false, nil
	case err != nil:
		return domain.ImportStamp{}, false, fmt.Errorf("catalog: find an earlier add import: %w", err)
	}
	return stamp, true, nil
}

type storedChange struct {
	SKU         string `json:"sku"`
	StockBefore int    `json:"stock_before"`
	StockAfter  int    `json:"stock_after"`
	PriceBefore string `json:"price_before"`
	PriceAfter  string `json:"price_after"`
}

func encodeChanges(changes []domain.ImportChange) ([]byte, error) {
	out := make([]storedChange, len(changes))
	for i, c := range changes {
		out[i] = storedChange{SKU: c.SKU, StockBefore: c.StockBefore, StockAfter: c.StockAfter,
			PriceBefore: c.PriceBefore.StringFixed(2), PriceAfter: c.PriceAfter.StringFixed(2)}
	}
	return json.Marshal(out)
}

func decodeChanges(raw []byte) ([]domain.ImportChange, error) {
	var stored []storedChange
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	out := make([]domain.ImportChange, len(stored))
	for i, c := range stored {
		before, err := decimal.NewFromString(c.PriceBefore)
		if err != nil {
			return nil, err
		}
		after, err := decimal.NewFromString(c.PriceAfter)
		if err != nil {
			return nil, err
		}
		out[i] = domain.ImportChange{SKU: c.SKU, StockBefore: c.StockBefore, StockAfter: c.StockAfter, PriceBefore: before, PriceAfter: after}
	}
	return out, nil
}

func (l *ImportLog) Finish(ctx context.Context, id string, from domain.ImportStatus, report app.ImportReport) (bool, error) {
	errs, err := json.Marshal(report.Errors)
	if err != nil {
		return false, fmt.Errorf("catalog: encode the import errors: %w", err)
	}
	changes, err := encodeChanges(report.Changes)
	if err != nil {
		return false, fmt.Errorf("catalog: encode the import changes: %w", err)
	}
	tag, err := database.Q(ctx, l.pool).Exec(ctx,
		`UPDATE imports
		 SET status = 'APPLIED', applied_at = now(), records = $3, skipped = $4, created = $5,
		     updated = $6, unchanged = $7, rejected = $8, errors = $9, changes = $10
		 WHERE id = $1::uuid AND status = $2`,
		id, string(from), report.Records, report.Skipped, report.Created, report.Updated, report.Unchanged, report.Rejected,
		errs, changes)
	if err != nil {
		return false, fmt.Errorf("catalog: finish import: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
