package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txKey struct{}

type connKey struct{}

func WithConn(ctx context.Context, conn *pgxpool.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, conn)
}

func connFrom(ctx context.Context) (*pgxpool.Conn, bool) {
	conn, ok := ctx.Value(connKey{}).(*pgxpool.Conn)
	return conn, ok
}

type TxManager struct {
	pool *pgxpool.Pool
}

func NewTxManager(pool *pgxpool.Pool) (*TxManager, error) {
	if pool == nil {
		return nil, errors.New("database: tx manager requires a pool")
	}
	return &TxManager{pool: pool}, nil
}

func (m *TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := txFrom(ctx); ok {
		return fn(ctx)
	}

	var (
		tx  pgx.Tx
		err error
	)
	if conn, ok := connFrom(ctx); ok {
		tx, err = conn.Begin(ctx)
	} else {
		tx, err = m.pool.Begin(ctx)
	}
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}

	finished := false
	defer func() {
		if finished {
			return
		}
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		finished = true
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil {
			return errors.Join(err, fmt.Errorf("database: rollback: %w", rbErr))
		}
		return err
	}

	finished = true
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}

func Q(ctx context.Context, pool *pgxpool.Pool) Querier {
	if tx, ok := txFrom(ctx); ok {
		return tx
	}
	if conn, ok := connFrom(ctx); ok {
		return conn
	}
	return pool
}

func txFrom(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}
