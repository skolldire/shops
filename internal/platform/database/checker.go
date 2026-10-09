package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Checker struct {
	pool *pgxpool.Pool
}

func NewChecker(pool *pgxpool.Pool) (*Checker, error) {
	if pool == nil {
		return nil, errors.New("database: checker requires a pool")
	}
	return &Checker{pool: pool}, nil
}

func (c *Checker) Check(ctx context.Context) error {
	if err := c.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database: ping: %w", err)
	}
	return nil
}
