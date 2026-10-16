package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skolldire/shops/internal/ordering/internal/app"
	"github.com/skolldire/shops/internal/platform/database"
)

const (
	deadlockDetected = "40P01"
	lockNotAvailable = "55P03"
)

type Tx struct {
	manager *database.TxManager
}

func NewTx(manager *database.TxManager) (*Tx, error) {
	if manager == nil {
		return nil, errors.New("ordering: transactions require a tx manager")
	}
	return &Tx{manager: manager}, nil
}

func (t *Tx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	var fnErr error
	err := t.manager.WithinTx(ctx, func(ctx context.Context) error {
		fnErr = fn(ctx)
		return fnErr
	})
	if err != nil && errors.Is(err, fnErr) && errors.Is(fnErr, err) && retryable(fnErr) {
		return fmt.Errorf("%w: %w", app.ErrBusy, err)
	}
	return err
}

type ReservationTx struct {
	tx          *Tx
	pool        *pgxpool.Pool
	lockTimeout string
}

func NewReservationTx(manager *database.TxManager, pool *pgxpool.Pool, lockTimeout time.Duration) (*ReservationTx, error) {
	switch {
	case pool == nil:
		return nil, errors.New("ordering: the reservation transaction requires a pool")
	case lockTimeout < time.Millisecond:
		return nil, errors.New("ordering: the reservation lock timeout must be at least 1ms")
	}
	tx, err := NewTx(manager)
	if err != nil {
		return nil, err
	}
	return &ReservationTx{tx: tx, pool: pool, lockTimeout: strconv.FormatInt(lockTimeout.Milliseconds(), 10) + "ms"}, nil
}

func (r *ReservationTx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return r.tx.WithinTx(ctx, func(ctx context.Context) error {
		if _, err := database.Q(ctx, r.pool).Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, r.lockTimeout); err != nil {
			return fmt.Errorf("ordering: bound the reservation lock wait: %w", err)
		}
		return fn(ctx)
	})
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == deadlockDetected || pgErr.Code == lockNotAvailable)
}
