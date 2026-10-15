package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

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

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == deadlockDetected || pgErr.Code == lockNotAvailable)
}
