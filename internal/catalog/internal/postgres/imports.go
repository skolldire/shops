package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skolldire/shops/internal/catalog/internal/app"
	"github.com/skolldire/shops/internal/catalog/internal/domain"
	"github.com/skolldire/shops/internal/platform/database"
)

const previewLimitLock = "catalog.import.previews"

const importColumns = `id::text, COALESCE(idempotency_key::text, ''), mode, status, COALESCE(cancel_reason, ''),
file_name, file_bytes, file_hash, content, preview, records, skipped, created, updated, unchanged, rejected,
errors, preview_changed, created_at, applied_at, (status = 'PREVIEW' AND created_at < now() - $1 * interval '1 microsecond')`

type ImportLog struct {
	pool        *pgxpool.Pool
	tx          *database.TxManager
	ttl         time.Duration
	maxPreviews int
}

func NewImportLog(pool *pgxpool.Pool, ttl time.Duration, maxPreviews int) (*ImportLog, error) {
	switch {
	case pool == nil:
		return nil, errors.New("catalog: the import log requires a pool")
	case ttl <= 0 || maxPreviews < 1:
		return nil, errors.New("catalog: the import log requires a positive preview lifetime and limit")
	}
	tx, err := database.NewTxManager(pool)
	if err != nil {
		return nil, err
	}
	return &ImportLog{pool: pool, tx: tx, ttl: ttl, maxPreviews: maxPreviews}, nil
}

func (l *ImportLog) CreatePreview(ctx context.Context, rec app.ImportRecord) (app.ImportRecord, bool, error) {
	_, err := database.Q(ctx, l.pool).Exec(ctx,
		`UPDATE imports SET status = 'CANCELLED', cancel_reason = 'expired', content = NULL
		 WHERE status = 'PREVIEW' AND created_at < now() - $1 * interval '1 microsecond'`, l.ttl.Microseconds())
	if err != nil {
		return app.ImportRecord{}, false, fmt.Errorf("catalog: expire import previews: %w", err)
	}
	var (
		out     app.ImportRecord
		created bool
	)
	err = l.tx.WithinTx(ctx, func(ctx context.Context) error {
		q := database.Q(ctx, l.pool)
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, previewLimitLock); err != nil {
			return fmt.Errorf("catalog: lock the import previews: %w", err)
		}
		if rec.Key != "" {
			existing, err := l.byKey(ctx, rec.Key)
			if err == nil {
				out = existing
				return nil
			}
			if !errors.Is(err, app.ErrImportNotFound) {
				return err
			}
		}
		var active int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM imports WHERE status = 'PREVIEW' AND content IS NOT NULL`).Scan(&active); err != nil {
			return fmt.Errorf("catalog: count import previews: %w", err)
		}
		if active >= l.maxPreviews {
			return app.ErrTooManyPreviews
		}
		var err error
		out, created, err = l.insert(ctx, rec)
		return err
	})
	if err != nil {
		return app.ImportRecord{}, false, err
	}
	return out, created, nil
}

func (l *ImportLog) Create(ctx context.Context, rec app.ImportRecord) (app.ImportRecord, bool, error) {
	return l.insert(ctx, rec)
}

func (l *ImportLog) insert(ctx context.Context, rec app.ImportRecord) (app.ImportRecord, bool, error) {
	preview, err := json.Marshal(rec.Preview)
	if err != nil {
		return app.ImportRecord{}, false, fmt.Errorf("catalog: encode the import preview: %w", err)
	}
	var id string
	err = database.Q(ctx, l.pool).QueryRow(ctx,
		`INSERT INTO imports (idempotency_key, mode, status, file_name, file_bytes, file_hash, content, preview)
		 VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING id::text`,
		rec.Key, string(rec.Mode), string(rec.Status), rec.File.Name, rec.File.Bytes, rec.File.SHA256, rec.Content, preview).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := l.byKey(ctx, rec.Key)
		return existing, false, err
	}
	if err != nil {
		return app.ImportRecord{}, false, fmt.Errorf("catalog: insert import: %w", err)
	}
	created, err := l.Get(ctx, id)
	return created, true, err
}

func (l *ImportLog) Get(ctx context.Context, id string) (app.ImportRecord, error) {
	return l.one(ctx, `id = $2::uuid`, id)
}

func (l *ImportLog) byKey(ctx context.Context, key string) (app.ImportRecord, error) {
	return l.one(ctx, `idempotency_key = $2::uuid`, key)
}

func (l *ImportLog) one(ctx context.Context, where string, arg string) (app.ImportRecord, error) {
	row := database.Q(ctx, l.pool).QueryRow(ctx, `SELECT `+importColumns+` FROM imports WHERE `+where, l.ttl.Microseconds(), arg)
	rec, err := scanImport(row)
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
		preview, errs []byte
		appliedAt     *time.Time
		r             = &rec.Report
	)
	if err := row.Scan(&rec.ID, &rec.Key, &mode, &status, &rec.CancelReason,
		&rec.File.Name, &rec.File.Bytes, &rec.File.SHA256, &rec.Content, &preview,
		&r.Records, &r.Skipped, &r.Created, &r.Updated, &r.Unchanged, &r.Rejected,
		&errs, &r.PreviewChanged, &rec.Stamp.CreatedAt, &appliedAt, &rec.Expired); err != nil {
		return app.ImportRecord{}, err
	}
	rec.Mode, rec.Status = domain.ImportMode(mode), domain.ImportStatus(status)
	if appliedAt != nil {
		rec.Stamp.AppliedAt = *appliedAt
	}
	if preview != nil {
		if err := json.Unmarshal(preview, &rec.Preview); err != nil {
			return app.ImportRecord{}, fmt.Errorf("decode the import preview: %w", err)
		}
	}
	if err := json.Unmarshal(errs, &r.Errors); err != nil {
		return app.ImportRecord{}, fmt.Errorf("decode the import errors: %w", err)
	}
	if r.Errors == nil {
		r.Errors = []app.RowError{}
	}
	r.ID, r.Mode, r.File = rec.ID, rec.Mode, rec.File
	return rec, nil
}

func (l *ImportLog) LastAppliedAdd(ctx context.Context, hash, exceptID string) (domain.ImportStamp, bool, error) {
	var stamp domain.ImportStamp
	err := database.Q(ctx, l.pool).QueryRow(ctx,
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

func (l *ImportLog) StartApplying(ctx context.Context, id string) (bool, error) {
	return l.transition(ctx, `UPDATE imports SET status = 'APPLYING'
		WHERE id = $1::uuid AND status = 'PREVIEW' AND created_at >= now() - $2 * interval '1 microsecond'`, id, l.ttl.Microseconds())
}

func (l *ImportLog) Finish(ctx context.Context, id string, from domain.ImportStatus, report app.ImportReport) (bool, error) {
	errs, err := json.Marshal(report.Errors)
	if err != nil {
		return false, fmt.Errorf("catalog: encode the import errors: %w", err)
	}
	return l.transition(ctx, `UPDATE imports
		SET status = 'APPLIED', applied_at = now(), content = NULL, records = $3, skipped = $4, created = $5,
		    updated = $6, unchanged = $7, rejected = $8, errors = $9, preview_changed = $10
		WHERE id = $1::uuid AND status = $2`,
		id, string(from), report.Records, report.Skipped, report.Created, report.Updated, report.Unchanged, report.Rejected,
		errs, report.PreviewChanged)
}

func (l *ImportLog) Cancel(ctx context.Context, id, reason string) (bool, error) {
	return l.transition(ctx, `UPDATE imports SET status = 'CANCELLED', cancel_reason = $2, content = NULL
		WHERE id = $1::uuid AND status = 'PREVIEW'`, id, reason)
}

func (l *ImportLog) transition(ctx context.Context, sql string, args ...any) (bool, error) {
	tag, err := database.Q(ctx, l.pool).Exec(ctx, sql, args...)
	if err != nil {
		return false, fmt.Errorf("catalog: change the import status: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (l *ImportLog) Lock(ctx context.Context, id string) (context.Context, func(), error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: acquire a connection for the import lock: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('catalog.import:' || $1, 0))`, id); err != nil {
		conn.Release()
		return nil, nil, fmt.Errorf("catalog: lock the import: %w", err)
	}
	return database.WithConn(ctx, conn), func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended('catalog.import:' || $1, 0))`, id)
		conn.Release()
	}, nil
}
